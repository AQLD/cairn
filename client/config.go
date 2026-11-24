package client

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
)

// DefaultEndpoint is where a local engine listens (cairnd's default, and the
// port `cairn up` publishes).
const DefaultEndpoint = "http://127.0.0.1:7420"

// Config is how a client finds its engine. `cairn up` writes one for the
// engine it starts, so the CLI, `cairn mcp` and the SDKs find it with no
// environment set.
type Config struct {
	Endpoint string `json:"endpoint"`
	Token    string `json:"token,omitempty"`
	// CACert is a PEM file of certificate authorities to trust for an
	// https endpoint (a private CA), besides the system's.
	CACert string `json:"ca_cert,omitempty"`
}

// ConfigPath is $CAIRN_CONFIG, else $XDG_CONFIG_HOME/cairn/config.json, else
// ~/.config/cairn/config.json (the same on macOS, so every tool and SDK looks
// in one place).
func ConfigPath() string {
	if p := os.Getenv("CAIRN_CONFIG"); p != "" {
		return p
	}
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, "cairn", "config.json")
}

// LoadConfig resolves the engine to talk to: CAIRN_ENDPOINT and CAIRN_TOKEN
// win, then the config file, then DefaultEndpoint with no token. A token from
// the file is used only with the file's endpoint, never sent elsewhere.
func LoadConfig() (Config, error) {
	cfg := Config{Endpoint: os.Getenv("CAIRN_ENDPOINT"), Token: os.Getenv("CAIRN_TOKEN"), CACert: os.Getenv("CAIRN_CA_CERT")}
	if cfg.Endpoint != "" {
		return cfg, nil
	}
	file, err := ReadConfig(ConfigPath())
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return cfg, err
	case file.Endpoint != "":
		if cfg.Token == "" {
			cfg.Token = file.Token
		}
		if cfg.CACert == "" {
			cfg.CACert = file.CACert
		}
		cfg.Endpoint = file.Endpoint
		return cfg, nil
	}
	cfg.Endpoint = DefaultEndpoint
	return cfg, nil
}

// ReadConfig reads a config file.
func ReadConfig(path string) (Config, error) {
	var cfg Config
	data, err := os.ReadFile(path)
	if err != nil {
		return cfg, err
	}
	return cfg, json.Unmarshal(data, &cfg)
}

// WriteConfig writes a config file readable only by its owner (it holds the
// token), replacing any earlier one atomically.
func WriteConfig(path string, cfg Config) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// FromConfig returns a client for the engine LoadConfig resolves.
func FromConfig() (*Client, error) {
	cfg, err := LoadConfig()
	if err != nil {
		return nil, err
	}
	c := New(cfg.Endpoint, cfg.Token)
	if cfg.CACert != "" {
		return c.WithCA(cfg.CACert)
	}
	return c, nil
}
