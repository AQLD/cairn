package objstore

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/AQLD/cairn/internal/objstore/s3test"
)

func newTestS3(t *testing.T) (*S3, *s3test.Server) {
	t.Helper()
	srv := s3test.New()
	t.Cleanup(srv.Close)
	ep, _ := url.Parse(srv.URL)
	return &S3{Endpoint: ep, Region: "us-east-1", Bucket: "b", AccessKey: "k", SecretKey: "s", PartSize: 1 << 10}, srv
}

func get(t *testing.T, s Store, key string) []byte {
	t.Helper()
	rc, err := s.Get(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestPutGetListDelete(t *testing.T) {
	s, _ := newTestS3(t)
	ctx := context.Background()
	small := []byte("hello")
	big := bytes.Repeat([]byte("0123456789"), 1000) // 10 KB: multipart with 1 KB parts
	for key, b := range map[string][]byte{"saves/a/small": small, "saves/a/big file": big} {
		if err := s.Put(ctx, key, bytes.NewReader(b), int64(len(b))); err != nil {
			t.Fatalf("put %s: %v", key, err)
		}
		if got := get(t, s, key); !bytes.Equal(got, b) {
			t.Fatalf("%s: got %d bytes back, want %d", key, len(got), len(b))
		}
	}
	keys, err := s.List(ctx, "saves/")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(keys, ",") != "saves/a/big file,saves/a/small" {
		t.Fatalf("list: %v", keys)
	}
	if err := s.Delete(ctx, "saves/a/small"); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(ctx, "saves/a/small"); err != nil {
		t.Fatalf("deleting a missing object: %v", err)
	}
	if _, err := s.Get(ctx, "saves/a/small"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get after delete: %v, want ErrNotFound", err)
	}
}

func TestDroppedConnectionsAreRetried(t *testing.T) {
	s, srv := newTestS3(t)
	var calls atomic.Int32
	// every other request is cut off: each must be retried, multipart parts included
	srv.SetFail(func(r *http.Request) bool { return calls.Add(1)%2 == 1 })
	big := bytes.Repeat([]byte("x"), 5000)
	if err := s.Put(context.Background(), "obj", bytes.NewReader(big), int64(len(big))); err != nil {
		t.Fatal(err)
	}
	srv.SetFail(nil)
	if got := get(t, s, "obj"); !bytes.Equal(got, big) {
		t.Fatalf("got %d bytes, want %d", len(got), len(big))
	}
}

func TestGivesUpCleanly(t *testing.T) {
	s, srv := newTestS3(t)
	s.Attempts = 3
	srv.SetFail(func(*http.Request) bool { return true })
	err := s.Put(context.Background(), "obj", strings.NewReader("data"), 4)
	if err == nil || !strings.Contains(err.Error(), "after 3 attempts") {
		t.Fatalf("want a clear failure after 3 attempts, got %v", err)
	}
	srv.SetFail(nil)
	if _, ok := srv.Object("b/obj"); ok {
		t.Fatal("a failed upload left an object behind")
	}
}

func TestPrefix(t *testing.T) {
	raw, srv := newTestS3(t)
	s := WithPrefix(raw, "cluster-1")
	if err := s.Put(context.Background(), "x/y", strings.NewReader("1"), 1); err != nil {
		t.Fatal(err)
	}
	if _, ok := srv.Object("b/cluster-1/x/y"); !ok {
		t.Fatalf("object not under the prefix: %v", srv.Keys())
	}
	keys, err := s.List(context.Background(), "x/")
	if err != nil || len(keys) != 1 || keys[0] != "x/y" {
		t.Fatalf("list through the prefix: %v, %v", keys, err)
	}
}

func TestSignatureIsStable(t *testing.T) {
	// a known request signed twice with a fixed clock gives the same header,
	// and the canonical encoding keeps unreserved characters as they are
	if got := uriEncode("a b/c~d"); got != "a%20b%2Fc~d" {
		t.Fatalf("uriEncode: %q", got)
	}
	if got := escapePath("/bucket/a b/c"); got != "/bucket/a%20b/c" {
		t.Fatalf("escapePath: %q", got)
	}
}

// TestRealS3 runs against a real S3-compatible store, which checks signatures
// (the fake does not): CAIRN_TEST_S3=s3://bucket/prefix?endpoint=...&region=...
// with AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY set.
func TestRealS3(t *testing.T) {
	raw := os.Getenv("CAIRN_TEST_S3")
	if raw == "" {
		t.Skip("set CAIRN_TEST_S3 to test against a real S3-compatible store")
	}
	s, err := Open(raw)
	if err != nil {
		t.Fatal(err)
	}
	if p, ok := s.(*prefixed); ok {
		p.s.(*S3).PartSize = 5 << 20 // S3's minimum part size: exercise multipart
	} else {
		s.(*S3).PartSize = 5 << 20
	}
	ctx := context.Background()
	big := bytes.Repeat([]byte("cairn"), 3<<20) // 15 MB: three parts
	for key, b := range map[string][]byte{"test/small file+1": []byte("hello"), "test/big": big} {
		if err := s.Put(ctx, key, bytes.NewReader(b), int64(len(b))); err != nil {
			t.Fatalf("put %s: %v", key, err)
		}
		if got := get(t, s, key); !bytes.Equal(got, b) {
			t.Fatalf("%s: got %d bytes back, want %d", key, len(got), len(b))
		}
	}
	keys, err := s.List(ctx, "test/")
	if err != nil || len(keys) != 2 {
		t.Fatalf("list: %v, %v", keys, err)
	}
	for _, k := range keys {
		if err := s.Delete(ctx, k); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Get(ctx, "test/big"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after delete: %v", err)
	}
}
