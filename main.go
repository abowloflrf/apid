// Command apid 是一个常驻的 LLM API 协议转换服务：
// 对外提供 OpenAI Responses API，对内转发给远程 Chat Completions 服务。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"
	"time"

	"github.com/abowloflrf/apid/internal/config"
	"github.com/abowloflrf/apid/internal/server"
	"github.com/abowloflrf/apid/internal/store"
)

// usageText explains how to run apid. apid has no command-line flags of its own:
// forwarding config lives in a TOML file and ops params come from env vars, so we
// list both here to make `apid --help` self-explanatory.
const usageText = `apid — a resident LLM API gateway: dispatches routes by model and converts protocols on demand.

Usage:
  apid                    start the gateway (runs in the foreground)
  apid --config PATH      start with an explicit config file (default config.toml)
  apid --help             print this help and exit
  apid --version          print the version and exit

Forwarding config (a TOML file at --config, default ./config.toml; missing file = startup failure):
  [[upstream]]  a backend, reused by name: name / protocol / base_url / path / api_key / model
  [[route]]     an entrypoint, dispatched by request model: path / input_protocol / [[route.model]]{match, upstream}
  See config.example.toml in the repo for a full example.

Ops params (environment variables; ./.env is loaded first, real env vars take precedence):
  APID_LISTEN     listen address (default :8080)
  APID_ENV_FILE   path to the .env file (default .env)
  APID_TRACE_DIR  directory for TRACE dumps; takes precedence when set
  APID_TRACE      when truthy (1/true/yes/on), dump TRACE to ./logs (default off)
  APID_DB         SQLite database file path; records request metrics async when set (empty = off)
`

// buildVersion reports the module version baked in at build time (set by
// `go install module@version` or VCS stamping), falling back to "dev" for plain
// local builds where no version is available.
func buildVersion() string {
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}
	return "dev"
}

func main() {
	flag.Usage = func() { fmt.Fprint(os.Stderr, usageText) }
	configPath := flag.String("config", "config.toml", "path to the TOML config file")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println("apid", buildVersion())
		return
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("config load failed: %v", err)
	}

	// 通用 SQLite 存储。cfg.DB 为空时 store.Open 返回 (nil, nil)，
	// 表示不启用，server 里所有持久化调用 nil-check 后跳过。
	st, err := store.Open(cfg.DB)
	if err != nil {
		log.Fatalf("store open failed: %v", err)
	}

	srv := server.New(cfg, st)

	httpServer := &http.Server{
		Addr:    cfg.Listen,
		Handler: srv.Handler(),
	}

	// 监听退出信号，优雅关闭。Shutdown 会先关 listener（让 ListenAndServe 立即
	// 返回），再阻塞等待 in-flight 请求跑完；只有它返回后所有 handler 的指标才都已
	// 入队，关 stats channel 才安全。用 idleClosed 把这个时序同步给主 goroutine。
	idleClosed := make(chan struct{})
	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		<-sigCh
		log.Println("shutdown signal received, stopping server...")
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(ctx)
		close(idleClosed)
	}()

	log.Printf("apid started: listening on %s, %d upstream(s), %d route(s)",
		cfg.Listen, len(cfg.Upstreams), len(cfg.Routes))
	for _, u := range cfg.Upstreams {
		log.Printf("  upstream %q -> %s%s [%s]", u.Name, u.BaseURL, u.Path, u.Protocol)
	}
	for _, rt := range cfg.Routes {
		for _, m := range rt.Models {
			match := m.Match
			if match == "" {
				match = "*"
			}
			log.Printf("  route %s [%s] model %q -> upstream %q",
				rt.Path, rt.InputProtocol, match, m.Upstream)
		}
	}
	if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("server exited unexpectedly: %v", err)
	}
	// 等 Shutdown 真正排空 in-flight 请求（它们的指标此时已全部入队），
	// 再排空 stats channel、最后关 SQLite，指标就不会丢、也不会向已关闭 channel 发送。
	<-idleClosed
	srv.Close()
	if err := st.Close(); err != nil {
		log.Printf("store close: %v", err)
	}
	log.Println("server stopped")
}
