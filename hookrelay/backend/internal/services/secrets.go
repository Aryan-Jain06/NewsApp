// Package services holds the business logic that sits between handlers/workers
// and the repositories.
package services

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
)

const (
	// APIKeyPrefix marks a HookRelay tenant API key.
	APIKeyPrefix = "hrk_"
	// EndpointSecretPrefix marks an endpoint signing secret, mirroring the
	// convention receivers already know from other webhook providers.
	EndpointSecretPrefix = "whsec_"
)

// randomToken returns n cryptographically random bytes as URL-safe base64.
func randomToken(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("read random bytes: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// GenerateAPIKey returns a new raw API key. It is shown to the user exactly once;
// only HashAPIKey's digest is persisted.
func GenerateAPIKey() (string, error) {
	tok, err := randomToken(32)
	if err != nil {
		return "", err
	}
	return APIKeyPrefix + tok, nil
}

// HashAPIKey returns the hex SHA-256 of a raw key. A plain digest (not bcrypt) is
// deliberate: authentication must look the key up in one indexed query, and the
// key is already 256 bits of entropy, so there is nothing for a salt to protect
// against.
func HashAPIKey(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// APIKeyDisplayPrefix returns the leading fragment safe to store and display so
// a user can tell two keys apart without revealing either.
func APIKeyDisplayPrefix(raw string) string {
	const n = 12
	if len(raw) <= n {
		return raw
	}
	return raw[:n]
}

// GenerateEndpointSecret returns a new whsec_ signing secret.
func GenerateEndpointSecret() (string, error) {
	tok, err := randomToken(24)
	if err != nil {
		return "", err
	}
	return EndpointSecretPrefix + tok, nil
}
