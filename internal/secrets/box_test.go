package secrets

import (
	"bytes"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

func TestSealOpen(t *testing.T) {
	key := bytes.Repeat([]byte{7}, 32)
	b, err := New(key)
	if err != nil {
		t.Fatal(err)
	}
	sealed := b.Seal("GITHUB_TOKEN", []byte("ghp_value"))
	if bytes.Contains(sealed, []byte("ghp_value")) {
		t.Fatal("sealed value holds the plaintext")
	}
	v, err := b.Open("GITHUB_TOKEN", sealed)
	if err != nil || string(v) != "ghp_value" {
		t.Fatalf("open: %q %v", v, err)
	}
	if _, err := b.Open("OTHER", sealed); err != ErrWrongKey {
		t.Fatalf("opened under another name: %v", err)
	}
	other, _ := New(bytes.Repeat([]byte{8}, 32))
	if _, err := other.Open("GITHUB_TOKEN", sealed); err != ErrWrongKey {
		t.Fatalf("opened with another key: %v", err)
	}
}

func TestLoadKey(t *testing.T) {
	dir := t.TempDir()
	key := bytes.Repeat([]byte{9}, 32)
	for name, data := range map[string][]byte{"raw": key, "hex": []byte(hex.EncodeToString(key) + "\n")} {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, data, 0o600); err != nil {
			t.Fatal(err)
		}
		k, err := LoadKey(p)
		if err != nil || !bytes.Equal(k, key) {
			t.Fatalf("%s: %x %v", name, k, err)
		}
	}
	p := filepath.Join(dir, "short")
	_ = os.WriteFile(p, []byte("abc"), 0o600)
	if _, err := LoadKey(p); err == nil {
		t.Fatal("short key accepted")
	}
}
