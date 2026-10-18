// Package ocitest is an in-memory container registry for tests: images built
// from tar entries, anonymous, Basic or Bearer-token access, multi-platform
// indexes, and corrupted blobs on demand.
package ocitest

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
)

// Auth is how the registry admits clients.
type Auth int

const (
	Anonymous Auth = iota
	Basic          // HTTP Basic on every request
	Token          // Bearer tokens from /token, which takes Basic credentials
)

type Registry struct {
	*httptest.Server
	Auth               Auth
	Username, Password string

	mu        sync.Mutex
	blobs     map[string][]byte
	manifests map[string]map[string]stored // repo -> tag or digest -> manifest
	corrupt   map[string]bool
	token     string
	Requests  int
}

type stored struct {
	mediaType string
	data      []byte
}

func New() *Registry {
	r := &Registry{blobs: map[string][]byte{}, manifests: map[string]map[string]stored{}, corrupt: map[string]bool{}, token: "test-token"}
	r.Server = httptest.NewServer(http.HandlerFunc(r.serve))
	return r
}

// Host is the registry's host:port, for image references.
func (r *Registry) Host() string { return strings.TrimPrefix(r.URL, "http://") }

// Entry is one file in a layer. Type is tar's Typeflag (0 = regular file);
// Whiteout marks a deletion of Name ("dir/.wh.x" is written for "dir/x");
// Opaque marks folder Name as opaque.
type Entry struct {
	Name     string
	Body     string
	Mode     int64
	Type     byte
	Linkname string
	Whiteout bool
	Opaque   bool
}

// Layer builds a layer tar from entries (uncompressed).
func Layer(entries ...Entry) []byte {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		name := e.Name
		switch {
		case e.Whiteout:
			i := strings.LastIndex(name, "/")
			name = name[:i+1] + ".wh." + name[i+1:]
		case e.Opaque:
			name = strings.TrimSuffix(name, "/") + "/.wh..wh..opq"
		}
		hdr := &tar.Header{Name: name, Mode: e.Mode, Typeflag: e.Type, Linkname: e.Linkname, Size: int64(len(e.Body))}
		if hdr.Mode == 0 {
			hdr.Mode = 0o644
			if e.Type == tar.TypeDir {
				hdr.Mode = 0o755
			}
		}
		if hdr.Typeflag == 0 {
			hdr.Typeflag = tar.TypeReg
		}
		if hdr.Typeflag != tar.TypeReg {
			hdr.Size = 0
		}
		if err := tw.WriteHeader(hdr); err != nil {
			panic(err)
		}
		if hdr.Typeflag == tar.TypeReg {
			if _, err := tw.Write([]byte(e.Body)); err != nil {
				panic(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		panic(err)
	}
	return buf.Bytes()
}

// Gzip compresses a layer.
func Gzip(layer []byte) []byte {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	_, _ = zw.Write(layer)
	_ = zw.Close()
	return buf.Bytes()
}

// Push stores an image built from layers (gzip-compressed if gz) under
// repo:tag and returns its manifest digest.
func (r *Registry) Push(repo, tag string, gz bool, layers ...[]byte) string {
	d := r.pushManifest(repo, layers, gz, "application/vnd.oci.image.layer.v1.tar")
	r.tag(repo, tag, d)
	return d
}

// PushIndex stores a multi-platform image: one single-layer image per
// "os/arch" key, under repo:tag. Returns the index digest.
func (r *Registry) PushIndex(repo, tag string, perPlatform map[string][]byte) string {
	type desc struct {
		MediaType string            `json:"mediaType"`
		Digest    string            `json:"digest"`
		Size      int               `json:"size"`
		Platform  map[string]string `json:"platform"`
	}
	var list []desc
	for plat, layer := range perPlatform {
		os, arch, _ := strings.Cut(plat, "/")
		d := r.pushManifest(repo, [][]byte{layer}, true, "")
		list = append(list, desc{"application/vnd.oci.image.manifest.v1+json", d, len(r.manifests[repo][d].data), map[string]string{"os": os, "architecture": arch}})
	}
	data, _ := json.Marshal(map[string]any{"schemaVersion": 2, "mediaType": "application/vnd.oci.image.index.v1+json", "manifests": list})
	d := r.put(repo, "application/vnd.oci.image.index.v1+json", data)
	r.tag(repo, tag, d)
	return d
}

// PushRaw stores a manifest with any layer media types (to test refusals).
func (r *Registry) PushRaw(repo, tag, layerMediaType string, layer []byte) string {
	d := r.pushManifest(repo, [][]byte{layer}, false, layerMediaType)
	r.tag(repo, tag, d)
	return d
}

func (r *Registry) pushManifest(repo string, layers [][]byte, gz bool, plainType string) string {
	var descs []map[string]any
	for _, l := range layers {
		mt := plainType
		if mt == "" {
			mt = "application/vnd.oci.image.layer.v1.tar"
		}
		if gz {
			l, mt = Gzip(l), "application/vnd.oci.image.layer.v1.tar+gzip"
		}
		descs = append(descs, map[string]any{"mediaType": mt, "digest": r.blob(l), "size": len(l)})
	}
	cfg := []byte(`{"architecture":"amd64","os":"linux"}`)
	data, _ := json.Marshal(map[string]any{
		"schemaVersion": 2, "mediaType": "application/vnd.oci.image.manifest.v1+json",
		"config": map[string]any{"mediaType": "application/vnd.oci.image.config.v1+json", "digest": r.blob(cfg), "size": len(cfg)},
		"layers": descs,
	})
	return r.put(repo, "application/vnd.oci.image.manifest.v1+json", data)
}

// Corrupt makes the registry serve wrong bytes for a blob.
func (r *Registry) Corrupt(digest string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.corrupt[digest] = true
}

// LayerDigests returns the layer digests of repo:tag's manifest.
func (r *Registry) LayerDigests(repo, tag string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var m struct {
		Layers []struct{ Digest string } `json:"layers"`
	}
	_ = json.Unmarshal(r.manifests[repo][tag].data, &m)
	var out []string
	for _, l := range m.Layers {
		out = append(out, l.Digest)
	}
	return out
}

func digest(b []byte) string {
	s := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(s[:])
}

func (r *Registry) blob(b []byte) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	d := digest(b)
	r.blobs[d] = b
	return d
}

func (r *Registry) put(repo, mediaType string, data []byte) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.manifests[repo] == nil {
		r.manifests[repo] = map[string]stored{}
	}
	d := digest(data)
	r.manifests[repo][d] = stored{mediaType, data}
	return d
}

func (r *Registry) tag(repo, tag, d string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.manifests[repo][tag] = r.manifests[repo][d]
}

func (r *Registry) serve(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	r.Requests++
	r.mu.Unlock()
	if req.URL.Path == "/token" {
		user, pass, _ := req.BasicAuth()
		if r.Username != "" && (user != r.Username || pass != r.Password) {
			http.Error(w, "bad credentials", http.StatusUnauthorized)
			return
		}
		if !strings.Contains(req.URL.Query().Get("scope"), ":pull") {
			http.Error(w, "no pull scope", http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"token": r.token})
		return
	}
	if !r.admitted(req) {
		switch r.Auth {
		case Basic:
			w.Header().Set("WWW-Authenticate", `Basic realm="test"`)
		case Token:
			w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer realm="%s/token",service="ocitest"`, r.URL))
		}
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	rest, ok := strings.CutPrefix(req.URL.Path, "/v2/")
	if !ok {
		http.NotFound(w, req)
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if i := strings.LastIndex(rest, "/manifests/"); i >= 0 {
		m, ok := r.manifests[rest[:i]][rest[i+len("/manifests/"):]]
		if !ok {
			http.NotFound(w, req)
			return
		}
		w.Header().Set("Content-Type", m.mediaType)
		_, _ = w.Write(m.data)
		return
	}
	if i := strings.LastIndex(rest, "/blobs/"); i >= 0 {
		d := rest[i+len("/blobs/"):]
		b, ok := r.blobs[d]
		if !ok {
			http.NotFound(w, req)
			return
		}
		if r.corrupt[d] {
			b = append([]byte{}, b...)
			b[len(b)/2] ^= 0xff
		}
		_, _ = w.Write(b)
		return
	}
	http.NotFound(w, req)
}

func (r *Registry) admitted(req *http.Request) bool {
	switch r.Auth {
	case Basic:
		u, p, ok := req.BasicAuth()
		return ok && u == r.Username && p == r.Password
	case Token:
		return req.Header.Get("Authorization") == "Bearer "+r.token
	}
	return true
}
