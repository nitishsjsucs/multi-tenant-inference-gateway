// Package auth maps API keys to tenants. Keys are never stored: the config
// holds their SHA-256 digests and incoming keys are hashed before lookup.
package auth

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"

	"github.com/nitishsjsucs/multi-tenant-inference-gateway/internal/config"
)

// ErrMissingKey and ErrInvalidKey distinguish absent from unknown credentials.
var (
	ErrMissingKey = errors.New("missing API key: send it as \"Authorization: Bearer <key>\"")
	ErrInvalidKey = errors.New("incorrect API key provided")
)

// HashKey returns the hex SHA-256 digest stored in the config for key.
func HashKey(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

// Authenticator resolves bearer tokens to tenants.
type Authenticator struct {
	byDigest map[string]*config.Tenant
}

// New indexes the tenants' key digests.
func New(tenants []config.Tenant) *Authenticator {
	a := &Authenticator{byDigest: map[string]*config.Tenant{}}
	for i := range tenants {
		for _, digest := range tenants[i].APIKeys {
			a.byDigest[strings.ToLower(digest)] = &tenants[i]
		}
	}
	return a
}

// Authenticate returns the tenant owning the request's bearer token. Lookup
// is by digest, so response timing does not depend on how much of a guessed
// key matches a real one.
func (a *Authenticator) Authenticate(r *http.Request) (*config.Tenant, error) {
	scheme, key, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	key = strings.TrimSpace(key)
	if !ok || !strings.EqualFold(scheme, "Bearer") || key == "" {
		return nil, ErrMissingKey
	}
	t, ok := a.byDigest[HashKey(key)]
	if !ok {
		return nil, ErrInvalidKey
	}
	return t, nil
}
