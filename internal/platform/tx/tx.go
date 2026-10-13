// Package tx provides a unit-of-work helper: everything inside Do commits or rolls back together.
package tx

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ritel35k/backend/db/gen"
)

type Manager struct{ pool *pgxpool.Pool }

func NewManager(pool *pgxpool.Pool) *Manager { return &Manager{pool: pool} }

// Queries returns non-transactional queries on the pool (reads).
func (m *Manager) Queries() *gen.Queries { return gen.New(m.pool) }

func (m *Manager) Pool() *pgxpool.Pool { return m.pool }

// Do runs fn inside a transaction. Any error (or panic) rolls back.
func (m *Manager) Do(ctx context.Context, fn func(q *gen.Queries) error) (err error) {
	t, err := m.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() {
		if p := recover(); p != nil {
			_ = t.Rollback(ctx)
			panic(p)
		}
		if err != nil {
			_ = t.Rollback(ctx)
		}
	}()
	if err = fn(gen.New(t)); err != nil {
		return err
	}
	if err = t.Commit(ctx); err != nil {
		return fmt.Errorf("commit tx: %w", err)
	}
	return nil
}
