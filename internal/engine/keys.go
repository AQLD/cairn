package engine

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/AQLD/cairn/internal/store"
)

// API keys: each caller (an agent, a CI job, a person) gets its own key,
// scoped and revocable without touching anyone else's. The engine keeps only
// a key's SHA-256; the token itself is shown once, when the key is made.

// Key scopes, from least to most.
const (
	ScopeRead  = "read"  // look, never change
	ScopeWrite = "write" // use workspaces: create, run, write, save, resume, copy, delete
	ScopeAdmin = "admin" // everything, keys, secrets and templates included
)

// Key is what the API shows of a key: never its token.
type Key struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Scope string `json:"scope"`
	// Own keys see and use only the workspaces they made (and their saves).
	Own       bool       `json:"own,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
	RevokedAt *time.Time `json:"revoked_at,omitempty"`
	// Token is set only in the answer that created the key.
	Token string `json:"token,omitempty"`
}

// ErrBadKey means a token that is no key, or a revoked one's.
var ErrBadKey = errors.New("unknown or revoked API key")

const tokenPrefix = "cairn_"

func hashToken(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:])
}

// CreateKey makes a key and returns it with its token, the only time the
// token is ever shown.
func (e *Engine) CreateKey(ctx context.Context, name, scope string, own bool) (Key, error) {
	switch scope {
	case ScopeRead, ScopeWrite, ScopeAdmin:
	default:
		return Key{}, fmt.Errorf("%w: scope %q: want read, write or admin", ErrInvalid, scope)
	}
	if strings.TrimSpace(name) == "" || len(name) > 200 {
		return Key{}, fmt.Errorf("%w: a key needs a name (who or what uses it)", ErrInvalid)
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return Key{}, err
	}
	token := tokenPrefix + hex.EncodeToString(raw)
	if own && scope == ScopeAdmin {
		return Key{}, fmt.Errorf("%w: an admin key manages the whole engine; it cannot be limited to its own workspaces", ErrInvalid)
	}
	k := store.APIKey{ID: newID("key"), Name: name, Scope: scope, Hash: hashToken(token), Own: own, CreatedAt: e.cfg.Now()}
	if err := e.store.InsertAPIKey(ctx, k); err != nil {
		return Key{}, err
	}
	e.log.Info("API key created", "key", k.ID, "name", name, "scope", scope)
	e.note(ctx, "", "key_create", fmt.Sprintf("key %s (%s) created, scope %s", k.ID, name, scope), nil, map[string]any{"new_key": k.ID, "scope": scope})
	out := publicKey(k)
	out.Token = token
	return out, nil
}

// ListKeys lists keys, revoked ones included, without tokens.
func (e *Engine) ListKeys(ctx context.Context) ([]Key, error) {
	list, err := e.store.ListAPIKeys(ctx)
	if err != nil {
		return nil, err
	}
	out := []Key{}
	for _, k := range list {
		out = append(out, publicKey(k))
	}
	return out, nil
}

// RevokeKey stops a key working, at once.
func (e *Engine) RevokeKey(ctx context.Context, id string) error {
	if err := e.store.RevokeAPIKey(ctx, id, e.cfg.Now()); err != nil {
		return err
	}
	e.log.Info("API key revoked", "key", id)
	e.note(ctx, "", "key_revoke", "key "+id+" revoked", nil, map[string]any{"revoked_key": id})
	return nil
}

// Authenticate returns the live key a token belongs to, or ErrBadKey.
func (e *Engine) Authenticate(ctx context.Context, token string) (Key, error) {
	if !strings.HasPrefix(token, tokenPrefix) {
		return Key{}, ErrBadKey
	}
	k, err := e.store.APIKeyByHash(ctx, hashToken(token))
	if errors.Is(err, store.ErrNotFound) || (err == nil && !k.RevokedAt.IsZero()) {
		return Key{}, ErrBadKey
	}
	if err != nil {
		return Key{}, err
	}
	return publicKey(k), nil
}

func publicKey(k store.APIKey) Key {
	out := Key{ID: k.ID, Name: k.Name, Scope: k.Scope, Own: k.Own, CreatedAt: k.CreatedAt}
	if !k.RevokedAt.IsZero() {
		t := k.RevokedAt
		out.RevokedAt = &t
	}
	return out
}

// Caller is who made a request: the operator's token ("" ID) or a key.
type Caller struct {
	KeyID string
	Scope string
	Own   bool // limited to the workspaces this key made
}

// Mine reports whether a caller may see a workspace with this owner.
func (c Caller) Mine(owner string) bool { return !c.Own || owner == c.KeyID }

// WorkspaceOwner returns the key that made a workspace ("" for the
// operator, or an engine without keys).
func (e *Engine) WorkspaceOwner(ctx context.Context, id string) (string, error) {
	w, err := e.store.GetWorkspace(ctx, id)
	if err != nil {
		return "", err
	}
	return w.Owner, nil
}

// SaveOwner returns the key that made the workspace a save belongs to.
func (e *Engine) SaveOwner(ctx context.Context, id string) (string, error) {
	sv, err := e.store.GetSave(ctx, id)
	if err != nil {
		return "", err
	}
	return e.WorkspaceOwner(ctx, sv.WorkspaceID)
}

// owner is the key acting now, recorded on what it creates.
func owner(ctx context.Context) string {
	c, _ := CallerOf(ctx)
	return c.KeyID
}

type callerKey struct{}

// WithCaller records who is acting, for the activity log.
func WithCaller(ctx context.Context, c Caller) context.Context {
	return context.WithValue(ctx, callerKey{}, c)
}

// CallerOf returns who is acting, if known.
func CallerOf(ctx context.Context) (Caller, bool) {
	c, ok := ctx.Value(callerKey{}).(Caller)
	return c, ok
}
