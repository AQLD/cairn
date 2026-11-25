package ws

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRoundTrip(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := Upgrade(w, r)
		if err != nil {
			t.Error(err)
			return
		}
		defer c.Close()
		for {
			typ, msg, err := c.ReadMessage()
			if err != nil {
				return
			}
			_ = c.WriteMessage(typ, append([]byte("echo:"), msg...))
		}
	}))
	defer srv.Close()
	c, err := Dial(context.Background(), srv.URL, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	big := bytes.Repeat([]byte("x"), 70000) // the 64-bit length form
	for _, m := range [][]byte{[]byte("hi"), bytes.Repeat([]byte("y"), 300), big} {
		if err := c.WriteMessage(Binary, m); err != nil {
			t.Fatal(err)
		}
		typ, got, err := c.ReadMessage()
		if err != nil || typ != Binary || !bytes.Equal(got, append([]byte("echo:"), m...)) {
			t.Fatalf("round trip of %d bytes: type %d, %d bytes, %v", len(m), typ, len(got), err)
		}
	}
	if err := c.WriteMessage(Text, []byte(`{"resize":1}`)); err != nil {
		t.Fatal(err)
	}
	if typ, got, _ := c.ReadMessage(); typ != Text || !strings.HasPrefix(string(got), "echo:") {
		t.Fatalf("text: %d %q", typ, got)
	}
}

func TestRefusedHandshake(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":{"code":"forbidden"}}`, http.StatusForbidden)
	}))
	defer srv.Close()
	_, err := Dial(context.Background(), srv.URL, nil, nil)
	he, ok := err.(*HandshakeError)
	if !ok || he.Response.StatusCode != http.StatusForbidden {
		t.Fatalf("got %v", err)
	}
}
