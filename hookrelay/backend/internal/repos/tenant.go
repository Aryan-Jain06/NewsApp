package repos

import (
	"context"
	"fmt"

	"github.com/aryan-jain06/hookrelay/backend/internal/models"
	"github.com/google/uuid"
)

// TenantRepo reads and writes tenants.
type TenantRepo struct{ q Querier }

// Create inserts a tenant. apiKeyHash is a SHA-256 hex digest of the raw key;
// passwordHash is a bcrypt digest of the dashboard password.
func (r *TenantRepo) Create(ctx context.Context, name, email, passwordHash, apiKeyHash, apiKeyPrefix string) (*models.Tenant, error) {
	const q = `
		INSERT INTO tenants (name, email, password_hash, api_key_hash, api_key_prefix)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING id, name, email, api_key_prefix, created_at`
	var t models.Tenant
	err := r.q.QueryRow(ctx, q, name, email, passwordHash, apiKeyHash, apiKeyPrefix).
		Scan(&t.ID, &t.Name, &t.Email, &t.APIKeyPrefix, &t.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("create tenant: %w", mapErr(err))
	}
	return &t, nil
}

// ByAPIKeyHash resolves a tenant from the hash of a presented API key.
func (r *TenantRepo) ByAPIKeyHash(ctx context.Context, hash string) (*models.Tenant, error) {
	const q = `
		SELECT id, name, email, api_key_prefix, created_at
		FROM tenants WHERE api_key_hash = $1`
	var t models.Tenant
	err := r.q.QueryRow(ctx, q, hash).Scan(&t.ID, &t.Name, &t.Email, &t.APIKeyPrefix, &t.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("tenant by api key: %w", mapErr(err))
	}
	return &t, nil
}

// ByEmailWithPassword returns the tenant plus its bcrypt hash for login checks.
func (r *TenantRepo) ByEmailWithPassword(ctx context.Context, email string) (*models.Tenant, string, error) {
	const q = `
		SELECT id, name, email, api_key_prefix, created_at, password_hash
		FROM tenants WHERE email = $1`
	var (
		t    models.Tenant
		hash string
	)
	err := r.q.QueryRow(ctx, q, email).
		Scan(&t.ID, &t.Name, &t.Email, &t.APIKeyPrefix, &t.CreatedAt, &hash)
	if err != nil {
		return nil, "", fmt.Errorf("tenant by email: %w", mapErr(err))
	}
	return &t, hash, nil
}

// ByID looks a tenant up by primary key.
func (r *TenantRepo) ByID(ctx context.Context, id uuid.UUID) (*models.Tenant, error) {
	const q = `
		SELECT id, name, email, api_key_prefix, created_at
		FROM tenants WHERE id = $1`
	var t models.Tenant
	err := r.q.QueryRow(ctx, q, id).Scan(&t.ID, &t.Name, &t.Email, &t.APIKeyPrefix, &t.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("tenant by id: %w", mapErr(err))
	}
	return &t, nil
}

// RotateAPIKey replaces the stored key hash.
func (r *TenantRepo) RotateAPIKey(ctx context.Context, id uuid.UUID, apiKeyHash, prefix string) error {
	const q = `UPDATE tenants SET api_key_hash = $2, api_key_prefix = $3 WHERE id = $1`
	tag, err := r.q.Exec(ctx, q, id, apiKeyHash, prefix)
	if err != nil {
		return fmt.Errorf("rotate api key: %w", mapErr(err))
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}
