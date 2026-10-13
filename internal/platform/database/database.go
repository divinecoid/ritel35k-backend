// Package database owns the pgx pool and goose migrations.
package database

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"github.com/ritel35k/backend/db"
)

func Connect(ctx context.Context, url string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, fmt.Errorf("parse database url: %w", err)
	}
	cfg.MaxConns = 20
	cfg.MaxConnLifetime = time.Hour
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}
	return pool, nil
}

func gooseDB(pool *pgxpool.Pool) (*sql.DB, error) {
	goose.SetBaseFS(db.Migrations)
	if err := goose.SetDialect("postgres"); err != nil {
		return nil, err
	}
	return stdlib.OpenDBFromPool(pool), nil
}

func MigrateUp(ctx context.Context, pool *pgxpool.Pool) error {
	sdb, err := gooseDB(pool)
	if err != nil {
		return err
	}
	defer sdb.Close()
	return goose.UpContext(ctx, sdb, "migrations")
}

func MigrateDown(ctx context.Context, pool *pgxpool.Pool) error {
	sdb, err := gooseDB(pool)
	if err != nil {
		return err
	}
	defer sdb.Close()
	return goose.DownContext(ctx, sdb, "migrations")
}

func MigrateReset(ctx context.Context, pool *pgxpool.Pool) error {
	sdb, err := gooseDB(pool)
	if err != nil {
		return err
	}
	defer sdb.Close()
	return goose.DownToContext(ctx, sdb, "migrations", 0)
}

func MigrateStatus(ctx context.Context, pool *pgxpool.Pool) error {
	sdb, err := gooseDB(pool)
	if err != nil {
		return err
	}
	defer sdb.Close()
	return goose.StatusContext(ctx, sdb, "migrations")
}

// PartitionedTables are the monthly range-partitioned tables (by created_at).
var PartitionedTables = []string{"audit_logs", "inventory_movements", "rfid_events"}

// EnsurePartitions creates monthly partitions for [now-1m, now+months] on every partitioned table.
func EnsurePartitions(ctx context.Context, pool *pgxpool.Pool, now time.Time, months int) error {
	start := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC).AddDate(0, -1, 0)
	for _, table := range PartitionedTables {
		for i := 0; i <= months+1; i++ {
			from := start.AddDate(0, i, 0)
			to := from.AddDate(0, 1, 0)
			name := fmt.Sprintf("%s_%s", table, from.Format("200601"))
			stmt := fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s PARTITION OF %s FOR VALUES FROM ('%s') TO ('%s')`,
				name, table, from.Format("2006-01-02"), to.Format("2006-01-02"))
			if _, err := pool.Exec(ctx, stmt); err != nil {
				return fmt.Errorf("create partition %s: %w", name, err)
			}
		}
	}
	return nil
}
