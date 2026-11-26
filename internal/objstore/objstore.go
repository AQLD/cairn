// Package objstore keeps saves in S3-compatible object storage (AWS S3,
// Cloudflare R2, DigitalOcean Spaces, Google Cloud Storage's S3 API, MinIO),
// so a save outlives the server that made it.
//
// It is a small client of its own (SigV4 signing, multipart upload, retries)
// rather than a cloud SDK: cairnd stays one small static binary, and retries
// and integrity are under our control. Integrity does not rest on the store
// anyway: every downloaded save is verified against its manifest's checksums.
package objstore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"
)

// ErrNotFound: no object with that key.
var ErrNotFound = errors.New("object not found")

// Store is what the engine needs from object storage.
type Store interface {
	// Put stores size bytes from r under key, replacing any object there.
	Put(ctx context.Context, key string, r io.ReaderAt, size int64) error
	// Get opens the object for reading.
	Get(ctx context.Context, key string) (io.ReadCloser, error)
	// Delete removes the object; a missing one is not an error.
	Delete(ctx context.Context, key string) error
	// List returns the keys under prefix.
	List(ctx context.Context, prefix string) ([]string, error)
	// String names the store (bucket and prefix) for logs.
	String() string
}

// Conditional is a Store that can write an object only if it is as the
// caller last saw it: what lets several engines agree on one owner of a
// workspace without a coordinator. S3 has it (If-Match and If-None-Match on
// PUT); a store without it cannot hold leases.
type Conditional interface {
	// GetVersion returns a small object's content and its version (ETag).
	GetVersion(ctx context.Context, key string) (data []byte, version string, err error)
	// PutIf stores data under key only if the object's current version is
	// ifMatch or, with ifMatch "", only if there is no object yet. It
	// returns the new version, or ErrPrecondition if the object was not as
	// expected. It is never retried (a lost answer to a write that landed
	// would turn its retry into a false ErrPrecondition): any other failure
	// leaves the outcome unknown, and the caller reads the object to learn it.
	PutIf(ctx context.Context, key string, data []byte, ifMatch string) (version string, err error)
}

// ErrPrecondition is PutIf finding the object not as expected.
var ErrPrecondition = errors.New("object changed")

// Open returns the store a URL names:
//
//	s3://bucket/prefix?endpoint=https://host&region=us-east-1
//
// endpoint defaults to AWS S3 for the region; credentials come from
// AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY and AWS_SESSION_TOKEN.
func Open(raw string) (Store, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, err
	}
	if u.Scheme != "s3" || u.Host == "" {
		return nil, fmt.Errorf("object store %q: want s3://bucket/prefix", raw)
	}
	q := u.Query()
	region := q.Get("region")
	if region == "" {
		region = "us-east-1"
	}
	endpoint := q.Get("endpoint")
	if endpoint == "" {
		endpoint = "https://s3." + region + ".amazonaws.com"
	}
	ep, err := url.Parse(endpoint)
	if err != nil || ep.Host == "" {
		return nil, fmt.Errorf("object store endpoint %q is not a URL", endpoint)
	}
	s := &S3{
		Endpoint: ep, Region: region, Bucket: u.Host,
		AccessKey: os.Getenv("AWS_ACCESS_KEY_ID"), SecretKey: os.Getenv("AWS_SECRET_ACCESS_KEY"), SessionToken: os.Getenv("AWS_SESSION_TOKEN"),
	}
	if s.AccessKey == "" || s.SecretKey == "" {
		return nil, errors.New("object store: set AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY")
	}
	return WithPrefix(s, strings.Trim(u.Path, "/")), nil
}

// WithPrefix returns a view of s that keeps every key under prefix.
func WithPrefix(s Store, prefix string) Store {
	if prefix == "" {
		return s
	}
	return &prefixed{s: s, p: prefix + "/"}
}

type prefixed struct {
	s Store
	p string
}

func (p *prefixed) Put(ctx context.Context, key string, r io.ReaderAt, size int64) error {
	return p.s.Put(ctx, p.p+key, r, size)
}
func (p *prefixed) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	return p.s.Get(ctx, p.p+key)
}
func (p *prefixed) Delete(ctx context.Context, key string) error { return p.s.Delete(ctx, p.p+key) }
func (p *prefixed) List(ctx context.Context, prefix string) ([]string, error) {
	keys, err := p.s.List(ctx, p.p+prefix)
	for i := range keys {
		keys[i] = strings.TrimPrefix(keys[i], p.p)
	}
	return keys, err
}
func (p *prefixed) String() string { return p.s.String() + "/" + strings.TrimSuffix(p.p, "/") }

func (p *prefixed) GetVersion(ctx context.Context, key string) ([]byte, string, error) {
	c, ok := p.s.(Conditional)
	if !ok {
		return nil, "", errNotConditional(p.s)
	}
	return c.GetVersion(ctx, p.p+key)
}

func (p *prefixed) PutIf(ctx context.Context, key string, data []byte, ifMatch string) (string, error) {
	c, ok := p.s.(Conditional)
	if !ok {
		return "", errNotConditional(p.s)
	}
	return c.PutIf(ctx, p.p+key, data, ifMatch)
}

func errNotConditional(s Store) error {
	return fmt.Errorf("object store %s cannot write conditionally", s)
}
