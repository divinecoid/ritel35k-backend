package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/ritel35k/backend/internal/app"
	"github.com/ritel35k/backend/internal/customer"
	"github.com/ritel35k/backend/internal/notification"
	"github.com/ritel35k/backend/internal/platform/config"
	"github.com/ritel35k/backend/internal/platform/database"
	"github.com/ritel35k/backend/internal/platform/logger"
	"github.com/ritel35k/backend/internal/rfid"
	"github.com/ritel35k/backend/internal/summary"
)

// Worker: outbox relay (domain events → consumers) and housekeeping jobs.
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

	a := app.New(cfg, pool, log)

	go func() { // keep monthly partitions ahead of time
		t := time.NewTicker(6 * time.Hour)
		defer t.Stop()
		for {
			if err := database.EnsurePartitions(ctx, pool, time.Now(), 3); err != nil {
				log.Error().Err(err).Msg("ensure partitions")
			}
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()

	go func() { // loyalty points expiry: daily, idempotent
		t := time.NewTicker(6 * time.Hour)
		defer t.Stop()
		for {
			if n, err := customer.ExpirePoints(ctx, a.Tx, time.Now()); err != nil {
				log.Error().Err(err).Msg("expire points")
			} else if n > 0 {
				log.Info().Int("customers", n).Msg("loyalty points expired")
			}
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()

	go func() { // summary layer: rebuild slices that sales/voids/refunds marked dirty (coalesced, every couple of seconds)
		t := time.NewTicker(2 * time.Second)
		defer t.Stop()
		for {
			for {
				n, err := summary.RebuildDirty(ctx, a.Tx, 50)
				if err != nil {
					if ctx.Err() == nil {
						log.Error().Err(err).Msg("summary rebuild")
					}
					break
				}
				if n == 0 {
					break
				}
				log.Debug().Int("slices", n).Msg("summary rebuilt")
			}
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()

	go func() { // daily reconcile: any slice whose summary disagrees with the source is re-marked and rebuilt
		t := time.NewTicker(24 * time.Hour)
		defer t.Stop()
		for {
			if n, err := summary.Reconcile(ctx, a.Tx, 7, time.Now()); err != nil {
				log.Error().Err(err).Msg("summary reconcile")
			} else if n > 0 {
				log.Warn().Int("slices", n).Msg("summary drift found and queued for rebuild")
			}
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()

	go func() { // low-stock alerts (once per store per day) and notification retention
		t := time.NewTicker(6 * time.Hour)
		defer t.Stop()
		for {
			if n, err := notification.CheckLowStock(ctx, a.Tx, time.Now()); err != nil {
				log.Error().Err(err).Msg("low stock check")
			} else if n > 0 {
				log.Info().Int("notifications", n).Msg("low stock notifications sent")
			}
			if _, err := notification.Purge(ctx, a.Tx, time.Now().AddDate(0, 0, -90)); err != nil {
				log.Error().Err(err).Msg("notification purge")
			}
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()

	go func() { // a gate that stopped reporting is the same as no gate: raise READER_OFFLINE
		t := time.NewTicker(time.Minute)
		defer t.Stop()
		for {
			if n, err := rfid.CheckReaders(ctx, a.Tx, time.Now()); err != nil {
				log.Error().Err(err).Msg("reader watchdog")
			} else if n > 0 {
				log.Warn().Int("readers", n).Msg("RFID readers offline")
			}
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()

	log.Info().Msg("worker started")
	a.Relay().Run(ctx, cfg.WorkerPoll)
	log.Info().Msg("worker stopped")
}
