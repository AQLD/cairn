// Package secrets seals secret values for the state store. Values are kept
// only encrypted (AES-256-GCM, the secret's name as associated data, so a
// sealed value cannot be moved to another name); the key never lives in the
// engine's data folder (docs/design/secrets.md).
package secrets

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
)

// Box seals and opens values with one key.
type Box struct{ aead cipher.AEAD }

// LoadKey reads a 32-byte key from a file: raw bytes, or hex or base64 text
// (openssl rand -hex 32).
func LoadKey(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(data) == 32 {
		return data, nil
	}
	text := bytes.TrimSpace(data)
	if k, err := hex.DecodeString(string(text)); err == nil && len(k) == 32 {
		return k, nil
	}
	if k, err := base64.StdEncoding.DecodeString(string(text)); err == nil && len(k) == 32 {
		return k, nil
	}
	return nil, fmt.Errorf("%s: want a 32-byte key (raw, hex or base64; openssl rand -hex 32)", path)
}

// New returns a box for a 32-byte key.
func New(key []byte) (*Box, error) {
	if len(key) != 32 {
		return nil, errors.New("secrets key must be 32 bytes")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Box{aead: aead}, nil
}

// Seal encrypts value for name: nonce followed by ciphertext.
func (b *Box) Seal(name string, value []byte) []byte {
	nonce := make([]byte, b.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		panic(err) // crypto/rand does not fail on supported systems
	}
	return b.aead.Seal(nonce, nonce, value, []byte(name))
}

// ErrWrongKey means a sealed value does not open: another key sealed it, or
// it was changed.
var ErrWrongKey = errors.New("secret does not open with this engine's key")

// Open decrypts what Seal returned for name.
func (b *Box) Open(name string, sealed []byte) ([]byte, error) {
	n := b.aead.NonceSize()
	if len(sealed) < n {
		return nil, ErrWrongKey
	}
	v, err := b.aead.Open(nil, sealed[:n], sealed[n:], []byte(name))
	if err != nil {
		return nil, ErrWrongKey
	}
	return v, nil
}
