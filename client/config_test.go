package client

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	t.Setenv("CAIRN_CONFIG", path)
	t.Setenv("CAIRN_ENDPOINT", "")
	t.Setenv("CAIRN_TOKEN", "")

	cfg, err := LoadConfig()
	if err != nil || cfg != (Config{Endpoint: DefaultEndpoint}) {
		t.Fatalf("no file: %+v %v", cfg, err)
	}

	if err := WriteConfig(path, Config{Endpoint: "http://127.0.0.1:9", Token: "file"}); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Fatalf("config mode %v, want 0600 (it holds the token)", fi.Mode().Perm())
	}
	if cfg, _ = LoadConfig(); cfg != (Config{Endpoint: "http://127.0.0.1:9", Token: "file"}) {
		t.Fatalf("file: %+v", cfg)
	}

	// an endpoint from the environment never gets the file's token
	t.Setenv("CAIRN_ENDPOINT", "http://elsewhere:7420")
	if cfg, _ = LoadConfig(); cfg != (Config{Endpoint: "http://elsewhere:7420"}) {
		t.Fatalf("env endpoint: %+v", cfg)
	}
	t.Setenv("CAIRN_ENDPOINT", "")
	t.Setenv("CAIRN_TOKEN", "env")
	if cfg, _ = LoadConfig(); cfg.Token != "env" {
		t.Fatalf("env token: %+v", cfg)
	}
}
