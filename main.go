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
)

func main() {
	cfg := config.Load()
	srv := server.New(cfg)

	httpServer := &http.Server{
		Addr:    cfg.Listen,
		Handler: srv.Handler(),
	}

	// 监听退出信号，优雅关闭。
	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		<-sigCh
		log.Println("shutdown signal received, stopping server...")
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(ctx)
	}()

	log.Printf("apid started: listening on %s, upstream %s", cfg.Listen, cfg.UpstreamBaseURL)
	if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("server exited unexpectedly: %v", err)
	}
	log.Println("server stopped")
}
