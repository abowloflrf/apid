// Command apid 是一个常驻的 LLM API 协议转换服务：
// 对外提供 OpenAI Responses API，对内转发给远程 Chat Completions 服务。
package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/abowloflrf/apid/internal/config"
	"github.com/abowloflrf/apid/internal/server"
	"github.com/abowloflrf/apid/internal/store"
)

func main() {
	cfg, err := config.Load()
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
