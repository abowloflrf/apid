// Command apid 是一个常驻的 LLM API 网关：
// 支持同协议纯转发，并在需要时执行已实现的协议转换。
package main

import (
	"cmp"
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"
	"time"

	"github.com/abowloflrf/apid/config"
	"github.com/abowloflrf/apid/server"
	"github.com/abowloflrf/apid/store"
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
  client_api_key optional static key for clients calling apid (empty/omitted = disabled)
  [[upstream]]  a backend, reused by name: name / protocol / base_url / path / api_key / model / auth_mode
  [[route]]     an entrypoint: path / input_protocol / operation / [[route.model]]{match, upstream}
  Protocols: openai_responses, openai_chat_completions, anthropic_messages.
  See config.example.toml in the repo for a full example.

Ops params (environment variables; ./.env is loaded first, real env vars take precedence):
  APID_LISTEN     listen address (default :19092)
  APID_ENV_FILE   path to the .env file (default .env)
  APID_TRACE_DIR  directory for TRACE dumps; takes precedence when set
  APID_TRACE      when truthy (1/true/yes/on), dump TRACE to ./logs (default off)
  APID_DB         SQLite database file path; records request metrics async when set (empty = off)
  APID_SHUTDOWN_TIMEOUT       graceful HTTP shutdown timeout (default 120s)
  APID_CODEX_SSE_IDLE_TIMEOUT  Codex subscription SSE idle timeout (default 5m)

Stats (read-only, only when APID_DB is set):
  GET /stats/        interactive dashboard UI (tokens / models / latency, filterable).
  GET /stats/daily   per-day, per-upstream-model usage as a flat JSON array (Grafana Infinity).
  Dashboard JSON API (shared params: from/to as Unix ms or RFC3339, tz_offset hour offset,
  model & protocol repeatable/comma-separated):
    /stats/summary /stats/by_model /stats/timeseries /stats/requests /stats/options
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

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	// Packages without an injected logger (e.g. convert) fall back to the default.
	slog.SetDefault(logger)

	if err := run(logger, *configPath); err != nil {
		logger.Error("apid exited", "err", err)
		os.Exit(1)
	}
}

// run wires everything together and blocks until shutdown completes. Exit
// ordering matters: Shutdown drains in-flight requests (so every handler has
// enqueued its metrics), then srv.Close drains the stats channel, and only
// then does the SQLite store close.
func run(logger *slog.Logger, configPath string) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	// Shared SQLite storage. With cfg.DB empty, store.Open returns (nil, nil)
	// meaning disabled; every persistence call downstream is nil-safe.
	st, err := store.Open(cfg.DB)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	if st.Enabled() {
		logger.Info("store enabled", "db", st.Path())
	}

	srv := server.New(cfg, st, logger)
	httpServer := &http.Server{
		Addr:              cfg.Listen,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() { errCh <- httpServer.ListenAndServe() }()

	logger.Info("apid started", "listen", cfg.Listen,
		"upstreams", len(cfg.Upstreams), "routes", len(cfg.Routes),
		"shutdown_timeout", cfg.ShutdownTimeout)
	for _, u := range cfg.Upstreams {
		attrs := []any{"name", u.Name, "endpoint", u.BaseURL + u.Path, "protocol", u.Protocol}
		if u.SupportsResponses {
			attrs = append(attrs, "responses_endpoint", u.BaseURL+u.EffectiveResponsesPath())
		}
		logger.Info("upstream", attrs...)
	}
	for _, rt := range cfg.Routes {
		for _, m := range rt.Models {
			logger.Info("route", "path", rt.Path, "protocol", rt.InputProtocol,
				"match", cmp.Or(m.Match, "*"), "upstream", m.Upstream)
		}
	}

	select {
	case err := <-errCh:
		srv.Close()
		_ = st.Close()
		return fmt.Errorf("http server: %w", err)
	case <-ctx.Done():
		stop()
		logger.Info("shutdown signal received, stopping server", "timeout", cfg.ShutdownTimeout)
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		logger.Warn("graceful shutdown incomplete", "err", err)
		_ = httpServer.Close()
	}
	srv.Close()
	if err := st.Close(); err != nil {
		logger.Warn("store close", "err", err)
	}
	logger.Info("server stopped")
	return nil
}
