// Package repos holds the SQL data-access layer. Every method takes a context
// and wraps its errors so callers can attach request-scoped detail.
package repos

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrNotFound is returned when a lookup matches no row.
var ErrNotFound = errors.New("not found")

// ErrConflict is returned when a write violates a unique constraint.
var ErrConflict = errors.New("conflict")

// Querier is satisfied by both *pgxpool.Pool and pgx.Tx, so repo methods can run
// inside or outside a transaction.
type Querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Store bundles every repository over one pool and provides transactions.
type Store struct {
	pool *pgxpool.Pool

	Tenants    *TenantRepo
	Endpoints  *EndpointRepo
	Events     *EventRepo
	Deliveries *DeliveryRepo
	Stats      *StatsRepo
}

// NewStore wires the repositories to a pool.
func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{
		pool:       pool,
		Tenants:    &TenantRepo{q: pool},
		Endpoints:  &EndpointRepo{q: pool},
		Events:     &EventRepo{q: pool},
		Deliveries: &DeliveryRepo{q: pool},
		Stats:      &StatsRepo{q: pool},
	}
}

// Pool exposes the underlying pool for health checks and metrics queries.
func (s *Store) Pool() *pgxpool.Pool { return s.pool }

// InTx runs fn inside a transaction, rolling back on error or panic.
func (s *Store) InTx(ctx context.Context, fn func(tx *TxStore) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			// Rollback on a cancelled context still needs a live context.
			_ = tx.Rollback(context.WithoutCancel(ctx))
		}
	}()

	txs := &TxStore{
		Tenants:    &TenantRepo{q: tx},
		Endpoints:  &EndpointRepo{q: tx},
		Events:     &EventRepo{q: tx},
		Deliveries: &DeliveryRepo{q: tx},
	}
	if err := fn(txs); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	committed = true
	return nil
}

// TxStore is the set of repositories bound to one transaction.
type TxStore struct {
	Tenants    *TenantRepo
	Endpoints  *EndpointRepo
	Events     *EventRepo
	Deliveries *DeliveryRepo
}

// isUniqueViolation reports whether err is a Postgres 23505.
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// mapErr normalises pgx sentinel errors onto the package's own.
func mapErr(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, pgx.ErrNoRows):
		return ErrNotFound
	case isUniqueViolation(err):
		return ErrConflict
	default:
		return err
	}
}
