package main

import (
	"context"
	"github.com/rafaelreverberi/twitchy-server/internal/server"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

func main() {
	cfg, e := server.LoadConfig()
	if e != nil {
		slog.Error("configuration invalid", "reason", e.Error())
		os.Exit(1)
	}
	app, e := server.New(cfg, server.StreamlinkProvider{Python: cfg.Python, Worker: cfg.Worker, CacheDir: filepath.Join(cfg.MediaDir, "..", "resolver")})
	if e != nil {
		slog.Error("startup failed")
		os.Exit(1)
	}
	h := &http.Server{Addr: cfg.Listen, Handler: app, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: cfg.Startup + 15*time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 16384}
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-stop
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = h.Shutdown(ctx)
		app.Close()
	}()
	slog.Info("source started", "version", server.Version)
	if e = h.ListenAndServe(); e != nil && e != http.ErrServerClosed {
		slog.Error("HTTP listener failed")
		app.Close()
		os.Exit(1)
	}
	app.Close()
}
