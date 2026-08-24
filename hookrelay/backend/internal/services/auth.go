package services

import (
	"context"
	"errors"
	"fmt"
	"net/mail"
	"strings"
	"time"

	"github.com/aryan-jain06/hookrelay/backend/internal/models"
	"github.com/aryan-jain06/hookrelay/backend/internal/repos"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"
)

// ErrInvalidCredentials is returned for a bad email/password pair. It is
// deliberately identical for "no such tenant" and "wrong password".
var ErrInvalidCredentials = errors.New("invalid credentials")

// TokenTTL is how long a dashboard JWT stays valid.
const TokenTTL = 12 * time.Hour

// AuthService issues and validates dashboard credentials and API keys.
type AuthService struct {
	tenants   *repos.TenantRepo
	jwtSecret []byte
}

// NewAuthService builds an AuthService.
func NewAuthService(tenants *repos.TenantRepo, jwtSecret string) *AuthService {
	return &AuthService{tenants: tenants, jwtSecret: []byte(jwtSecret)}
}

// RegisterResult carries the one-time secrets produced by registration.
type RegisterResult struct {
	Tenant *models.Tenant `json:"tenant"`
	APIKey string         `json:"api_key"`
	Token  string         `json:"token"`
}

// Register creates a tenant with a dashboard password and a fresh API key.
func (s *AuthService) Register(ctx context.Context, name, email, password string) (*RegisterResult, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	if _, err := mail.ParseAddress(email); err != nil {
		return nil, fmt.Errorf("invalid email address: %w", err)
	}
	if len(password) < 8 {
		return nil, errors.New("password must be at least 8 characters")
	}
	if strings.TrimSpace(name) == "" {
		name = email
	}

	pwHash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return nil, fmt.Errorf("hash password: %w", err)
	}
	apiKey, err := GenerateAPIKey()
	if err != nil {
		return nil, err
	}

	tenant, err := s.tenants.Create(ctx, name, email, string(pwHash), HashAPIKey(apiKey), APIKeyDisplayPrefix(apiKey))
	if err != nil {
		return nil, err
	}
	token, err := s.IssueToken(tenant)
	if err != nil {
		return nil, err
	}
	return &RegisterResult{Tenant: tenant, APIKey: apiKey, Token: token}, nil
}

// Login verifies a password and returns a dashboard JWT.
func (s *AuthService) Login(ctx context.Context, email, password string) (*models.Tenant, string, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	tenant, hash, err := s.tenants.ByEmailWithPassword(ctx, email)
	if err != nil {
		if errors.Is(err, repos.ErrNotFound) {
			// Still run a bcrypt comparison so a missing tenant and a wrong
			// password take similar time.
			_ = bcrypt.CompareHashAndPassword([]byte("$2a$10$invalidinvalidinvalidinvalidinvalidinvalidinvalidinvalidinv"), []byte(password))
			return nil, "", ErrInvalidCredentials
		}
		return nil, "", err
	}
	if err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)); err != nil {
		return nil, "", ErrInvalidCredentials
	}
	token, err := s.IssueToken(tenant)
	if err != nil {
		return nil, "", err
	}
	return tenant, token, nil
}

// IssueToken mints a signed JWT for a tenant.
func (s *AuthService) IssueToken(t *models.Tenant) (string, error) {
	now := time.Now()
	claims := jwt.RegisteredClaims{
		Subject:   t.ID.String(),
		Issuer:    "hookrelay",
		IssuedAt:  jwt.NewNumericDate(now),
		ExpiresAt: jwt.NewNumericDate(now.Add(TokenTTL)),
	}
	signed, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(s.jwtSecret)
	if err != nil {
		return "", fmt.Errorf("sign jwt: %w", err)
	}
	return signed, nil
}

// TenantFromToken validates a dashboard JWT and loads its tenant.
func (s *AuthService) TenantFromToken(ctx context.Context, raw string) (*models.Tenant, error) {
	var claims jwt.RegisteredClaims
	_, err := jwt.ParseWithClaims(raw, &claims, func(t *jwt.Token) (any, error) {
		if t.Method != jwt.SigningMethodHS256 {
			return nil, fmt.Errorf("unexpected signing method %q", t.Method.Alg())
		}
		return s.jwtSecret, nil
	}, jwt.WithIssuer("hookrelay"), jwt.WithExpirationRequired())
	if err != nil {
		return nil, fmt.Errorf("parse token: %w", err)
	}
	id, err := uuid.Parse(claims.Subject)
	if err != nil {
		return nil, fmt.Errorf("token subject is not a uuid: %w", err)
	}
	return s.tenants.ByID(ctx, id)
}

// TenantFromAPIKey resolves a raw API key to its tenant.
func (s *AuthService) TenantFromAPIKey(ctx context.Context, raw string) (*models.Tenant, error) {
	if !strings.HasPrefix(raw, APIKeyPrefix) {
		return nil, ErrInvalidCredentials
	}
	tenant, err := s.tenants.ByAPIKeyHash(ctx, HashAPIKey(raw))
	if err != nil {
		if errors.Is(err, repos.ErrNotFound) {
			return nil, ErrInvalidCredentials
		}
		return nil, err
	}
	return tenant, nil
}
