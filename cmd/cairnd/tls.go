package main

import (
	"crypto/tls"
	"fmt"
	"os"
	"sync"
	"time"
)

// certFiles serves a certificate and key from files, loading them again
// when either changes, so a renewed certificate (cert-manager, certbot) is
// used without restarting the engine. A renewal that does not load keeps the
// last good pair.
type certFiles struct {
	cert, key string

	mu      sync.Mutex
	current *tls.Certificate
	stamp   string
	checked time.Time
}

func newCertFiles(cert, key string) (*certFiles, error) {
	c := &certFiles{cert: cert, key: key}
	if err := c.reload(); err != nil {
		return nil, err
	}
	return c, nil
}

func (c *certFiles) fileStamp() string {
	var s string
	for _, p := range []string{c.cert, c.key} {
		if fi, err := os.Stat(p); err == nil {
			s += fmt.Sprintf("%d.%d;", fi.ModTime().UnixNano(), fi.Size())
		}
	}
	return s
}

func (c *certFiles) reload() error {
	stamp := c.fileStamp()
	pair, err := tls.LoadX509KeyPair(c.cert, c.key)
	if err != nil {
		return fmt.Errorf("TLS certificate %s / key %s: %w", c.cert, c.key, err)
	}
	c.current, c.stamp = &pair, stamp
	return nil
}

// get is tls.Config.GetCertificate; files are checked at most once a second.
func (c *certFiles) get(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if time.Since(c.checked) >= time.Second {
		c.checked = time.Now()
		if c.fileStamp() != c.stamp {
			_ = c.reload() // a half-written renewal: keep serving the old pair
		}
	}
	return c.current, nil
}

func (c *certFiles) config() *tls.Config {
	return &tls.Config{MinVersion: tls.VersionTLS12, GetCertificate: c.get}
}
