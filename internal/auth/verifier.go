// Package auth holds the access-code login, bearer keys and the middleware that
// resolves a principal for every request.
package auth

import (
	"context"
	"errors"
	"time"
)

// KeyKind is the kind of a bearer key. Only "mcp" keys are minted; the
// type stays so stored keys of another kind are recognised and refused.
type KeyKind string

const (
	KindMCP KeyKind = "mcp"
)

// KeyInfo describes a bearer key without its secret.
type KeyInfo struct {
	ID         string    `json:"id"`
	Label      string    `json:"label"`
	Kind       KeyKind   `json:"kind"`
	CreatedAt  time.Time `json:"created_at"`
	LastUsedAt time.Time `json:"last_used_at,omitempty"`
	// CanSend is the consent to send, given when the key was created or the
	// OAuth grant approved. Without it send_message is refused, even when the
	// bridge has sending on. Keys and grants from before this choice existed
	// have it off.
	CanSend bool `json:"can_send"`
	// OAuth is set when the credential is an OAuth access token; ID is then
	// the grant id.
	OAuth bool `json:"-"`
}

// ErrInvalidKey is returned for unknown, malformed or revoked keys.
var ErrInvalidKey = errors.New("auth: invalid key")

// Verifier checks a bearer credential. Implemented by *Store.
type Verifier interface {
	VerifyKey(ctx context.Context, bearer string) (KeyInfo, error)
}
