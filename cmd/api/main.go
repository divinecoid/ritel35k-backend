package main

import (
	"context"
	"errors"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/ritel35k/backend/internal/app"
	"github.com/ritel35k/backend/internal/bootstrap"
	"github.com/ritel35k/backend/internal/platform/config"
	"github.com/ritel35k/backend/internal/platform/database"
	"github.com/ritel35k/backend/internal/platform/logger"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		println("config:", err.Error())
		os.Exit(1)
	}
	log := logger.New(cfg.Env, cfg.LogLevel)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	pool, err := database.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		log.Fatal().Err(err).Msg("database")
	}
	defer pool.Close()

	if err := bootstrap.SyncPermissions(ctx, pool); err != nil {
		log.Fatal().Err(err).Msg("sync permissions")
	}

	a := app.New(cfg, pool, log)
	go a.Hub.Run(ctx, pool) // LISTEN/NOTIFY → SSE fan-out
	srv := &http.Server{Addr: cfg.HTTPAddr, Handler: a.Handler(), ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 60 * time.Second, IdleTimeout: 120 * time.Second}

	go func() {
		log.Info().Str("addr", cfg.HTTPAddr).Str("env", cfg.Env).Msg("api listening")
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatal().Err(err).Msg("http server")
		}
	}()
	<-ctx.Done()
	shutdown, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdown)
	log.Info().Msg("api stopped")
}
