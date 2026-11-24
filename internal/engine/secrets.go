package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/AQLD/cairn/internal/runtime"
	"github.com/AQLD/cairn/internal/secrets"
	"github.com/AQLD/cairn/internal/store"
)

// Secrets: credentials workspaces use without holding them
// (docs/design/secrets.md). This file keeps them: sealed in the store, never
// returned, never logged.

// Secret is what the API shows of a secret: never its value.
type Secret struct {
	Name      string    `json:"name"`
	Hosts     []string  `json:"hosts"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// checkName is a sealed known value that tells, at start, whether the key is
// the one the stored secrets were sealed with. Not a valid secret name.
const checkName = ".key-check"

var checkValue = []byte("cairn secrets key check")

// openSecrets sets up sealing with the configured key and refuses a key that
// does not open the secrets already stored.
func (e *Engine) openSecrets() error {
	if e.cfg.SecretsKey == nil {
		return nil
	}
	box, err := secrets.New(e.cfg.SecretsKey)
	if err != nil {
		return err
	}
	ctx := context.Background()
	check, err := e.store.GetSecret(ctx, checkName)
	switch {
	case errors.Is(err, store.ErrNotFound):
		now := e.cfg.Now()
		if err := e.store.PutSecret(ctx, store.Secret{Name: checkName, Sealed: box.Seal(checkName, checkValue), CreatedAt: now, UpdatedAt: now}); err != nil {
			return err
		}
	case err != nil:
		return err
	default:
		if _, err := box.Open(checkName, check.Sealed); err != nil {
			return errors.New("the secrets key does not open the secrets this engine stored (another key sealed them)")
		}
	}
	e.secrets = box
	return nil
}

// validSecretName: an environment variable name, so the workspace can use it as one.
func validSecretName(name string) bool {
	if name == "" || len(name) > 128 || (name[0] >= '0' && name[0] <= '9') {
		return false
	}
	return strings.Trim(name, "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789_") == ""
}

func (e *Engine) needSecrets() error {
	if e.secrets == nil {
		return fmt.Errorf("%w: secrets need a key (cairnd --secrets-key)", ErrUnsupported)
	}
	return nil
}

// SetSecret creates or replaces a secret: its value and the hosts it may be
// sent to (names, *.suffixes, IPv4 addresses or ranges).
func (e *Engine) SetSecret(ctx context.Context, name string, value []byte, hosts []string) (Secret, error) {
	if err := e.needSecrets(); err != nil {
		return Secret{}, err
	}
	if !validSecretName(name) {
		return Secret{}, fmt.Errorf("%w: secret name %q: letters, digits and _ only, not starting with a digit", ErrInvalid, name)
	}
	if len(value) == 0 {
		return Secret{}, fmt.Errorf("%w: secret %s has an empty value", ErrInvalid, name)
	}
	if len(hosts) == 0 {
		return Secret{}, fmt.Errorf("%w: secret %s needs at least one host it may be sent to", ErrInvalid, name)
	}
	if _, err := runtime.ParseEgress(hosts); err != nil {
		return Secret{}, fmt.Errorf("%w: secret %s: %v", ErrInvalid, name, err)
	}
	now := e.cfg.Now()
	created := now
	if old, err := e.store.GetSecret(ctx, name); err == nil {
		created = old.CreatedAt
	}
	sec := store.Secret{Name: name, Hosts: hosts, Sealed: e.secrets.Seal(name, value), CreatedAt: created, UpdatedAt: now}
	if err := e.store.PutSecret(ctx, sec); err != nil {
		return Secret{}, err
	}
	e.log.Info("secret set", "secret", name, "hosts", hosts) // never the value
	return publicSecret(sec), nil
}

// ListSecrets returns every secret, without values.
func (e *Engine) ListSecrets(ctx context.Context) ([]Secret, error) {
	list, err := e.store.ListSecrets(ctx)
	if err != nil {
		return nil, err
	}
	out := []Secret{}
	for _, s := range list {
		if s.Name != checkName {
			out = append(out, publicSecret(s))
		}
	}
	return out, nil
}

// DeleteSecret removes a secret.
func (e *Engine) DeleteSecret(ctx context.Context, name string) error {
	if !validSecretName(name) {
		return fmt.Errorf("%w: secret %q", ErrNotFound, name)
	}
	if err := e.store.DeleteSecret(ctx, name); err != nil {
		return err
	}
	e.log.Info("secret deleted", "secret", name)
	return nil
}

func publicSecret(s store.Secret) Secret {
	return Secret{Name: s.Name, Hosts: s.Hosts, CreatedAt: s.CreatedAt, UpdatedAt: s.UpdatedAt}
}
