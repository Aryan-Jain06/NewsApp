package services

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/aryan-jain06/hookrelay/backend/internal/models"
	"github.com/aryan-jain06/hookrelay/backend/internal/repos"
	"github.com/google/uuid"
)

// SecretRotationGrace is how long a rotated-out secret keeps verifying.
const SecretRotationGrace = 24 * time.Hour

// ErrValidation marks a caller mistake rather than a server fault.
var ErrValidation = errors.New("validation failed")

func validationErrorf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrValidation, fmt.Sprintf(format, args...))
}

// EndpointService implements endpoint CRUD, subscriptions and secret rotation.
type EndpointService struct {
	endpoints *repos.EndpointRepo
}

// NewEndpointService builds an EndpointService.
func NewEndpointService(endpoints *repos.EndpointRepo) *EndpointService {
	return &EndpointService{endpoints: endpoints}
}

// CreateEndpointInput is the request body for creating an endpoint.
type CreateEndpointInput struct {
	URL         string   `json:"url"`
	Description string   `json:"description"`
	EventTypes  []string `json:"event_types"`
	Active      *bool    `json:"active"`
}

// Create validates the input and inserts the endpoint with a fresh secret.
func (s *EndpointService) Create(ctx context.Context, tenantID uuid.UUID, in CreateEndpointInput) (*models.Endpoint, error) {
	if err := validateURL(in.URL); err != nil {
		return nil, err
	}
	types, err := normaliseEventTypes(in.EventTypes)
	if err != nil {
		return nil, err
	}
	secret, err := GenerateEndpointSecret()
	if err != nil {
		return nil, err
	}
	active := true
	if in.Active != nil {
		active = *in.Active
	}
	return s.endpoints.Create(ctx, tenantID, in.URL, strings.TrimSpace(in.Description), secret, active, types)
}

// List returns a tenant's endpoints with secrets stripped.
func (s *EndpointService) List(ctx context.Context, tenantID uuid.UUID) ([]*models.Endpoint, error) {
	out, err := s.endpoints.List(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	for _, e := range out {
		e.Secret = ""
	}
	return out, nil
}

// Get returns one endpoint. The signing secret is included only when reveal is
// true, so ordinary list/detail traffic never carries it.
func (s *EndpointService) Get(ctx context.Context, tenantID, id uuid.UUID, reveal bool) (*models.Endpoint, error) {
	e, err := s.endpoints.ByID(ctx, tenantID, id)
	if err != nil {
		return nil, err
	}
	if !reveal {
		e.Secret = ""
	}
	return e, nil
}

// UpdateEndpointInput is the PATCH body. Absent fields are left alone.
type UpdateEndpointInput struct {
	URL         *string   `json:"url"`
	Description *string   `json:"description"`
	Active      *bool     `json:"active"`
	EventTypes  *[]string `json:"event_types"`
}

// Update applies a partial update, optionally replacing the subscription set.
func (s *EndpointService) Update(ctx context.Context, tenantID, id uuid.UUID, in UpdateEndpointInput) (*models.Endpoint, error) {
	if in.URL != nil {
		if err := validateURL(*in.URL); err != nil {
			return nil, err
		}
	}
	var types []string
	if in.EventTypes != nil {
		var err error
		if types, err = normaliseEventTypes(*in.EventTypes); err != nil {
			return nil, err
		}
	}

	e, err := s.endpoints.Update(ctx, tenantID, id, in.URL, in.Description, in.Active)
	if err != nil {
		return nil, err
	}
	if in.EventTypes != nil {
		if err := s.endpoints.ReplaceSubscriptions(ctx, e.ID, types); err != nil {
			return nil, err
		}
		e.EventTypes = types
	} else if e.EventTypes, err = s.endpoints.EventTypes(ctx, e.ID); err != nil {
		return nil, err
	}
	e.Secret = ""
	return e, nil
}

// Delete removes an endpoint.
func (s *EndpointService) Delete(ctx context.Context, tenantID, id uuid.UUID) error {
	return s.endpoints.Delete(ctx, tenantID, id)
}

// RotateSecret installs a new signing secret and keeps the previous one valid
// for SecretRotationGrace so receivers can roll over without dropping events.
func (s *EndpointService) RotateSecret(ctx context.Context, tenantID, id uuid.UUID) (*models.Endpoint, error) {
	secret, err := GenerateEndpointSecret()
	if err != nil {
		return nil, err
	}
	e, err := s.endpoints.RotateSecret(ctx, tenantID, id, secret, time.Now().Add(SecretRotationGrace))
	if err != nil {
		return nil, err
	}
	if e.EventTypes, err = s.endpoints.EventTypes(ctx, e.ID); err != nil {
		return nil, err
	}
	return e, nil
}

// validateURL rejects anything that is not an absolute http(s) URL.
func validateURL(raw string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return validationErrorf("url is required")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return validationErrorf("url is not parseable: %v", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return validationErrorf("url scheme must be http or https, got %q", u.Scheme)
	}
	if u.Host == "" {
		return validationErrorf("url must include a host")
	}
	return nil
}

// normaliseEventTypes trims, lowercases, de-duplicates and sorts event types.
func normaliseEventTypes(in []string) ([]string, error) {
	if len(in) == 0 {
		return nil, validationErrorf("at least one event_type is required (use \"*\" to subscribe to all)")
	}
	seen := map[string]struct{}{}
	out := make([]string, 0, len(in))
	for _, t := range in {
		t = strings.ToLower(strings.TrimSpace(t))
		if t == "" {
			continue
		}
		if err := ValidateEventType(t); err != nil {
			return nil, err
		}
		if _, dup := seen[t]; dup {
			continue
		}
		seen[t] = struct{}{}
		out = append(out, t)
	}
	if len(out) == 0 {
		return nil, validationErrorf("at least one non-empty event_type is required")
	}
	sort.Strings(out)
	return out, nil
}

// ValidateEventType allows "*" plus dotted lowercase identifiers such as
// "order.created" or "invoice.payment_failed".
func ValidateEventType(t string) error {
	if t == "*" {
		return nil
	}
	if len(t) > 128 {
		return validationErrorf("event_type %q exceeds 128 characters", t)
	}
	for _, r := range t {
		ok := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '.' || r == '_' || r == '-'
		if !ok {
			return validationErrorf("event_type %q may only contain a-z, 0-9, '.', '_' and '-'", t)
		}
	}
	if strings.HasPrefix(t, ".") || strings.HasSuffix(t, ".") {
		return validationErrorf("event_type %q must not start or end with '.'", t)
	}
	return nil
}
