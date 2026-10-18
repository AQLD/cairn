// Package oci pulls container images from a registry (the OCI distribution
// API that Docker Hub, GHCR, ECR, Harbor and friends speak) and flattens them
// into a root filesystem: the source of templates built from an operator's own
// images. Pull only, standard library only.
package oci

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
	"net/url"
	"runtime"
	"strings"
)

var (
	// ErrInvalid: a malformed reference, or an image Cairn cannot use.
	ErrInvalid = errors.New("invalid image")
	// ErrNotFound: the registry has no such repository, tag or digest.
	ErrNotFound = errors.New("image not found")
	// ErrDenied: the registry refused the credentials (or needs some).
	ErrDenied = errors.New("registry denied access")
	// ErrCorrupt: something downloaded does not match its digest.
	ErrCorrupt = errors.New("image content does not match its digest")
)

// Reference is a parsed image reference: [host[:port]/]repository[:tag|@digest].
type Reference struct {
	Host       string // registry API host, e.g. ghcr.io or registry-1.docker.io
	Repository string // e.g. team/python
	Tag        string
	Digest     string // sha256:..., preferred over Tag when set
}

func (r Reference) String() string {
	s := r.Host + "/" + r.Repository
	if r.Digest != "" {
		return s + "@" + r.Digest
	}
	return s + ":" + r.Tag
}

// ParseReference parses an image reference the way docker does: a first part
// with a dot, a colon or "localhost" is the registry; otherwise Docker Hub.
func ParseReference(s string) (Reference, error) {
	var r Reference
	if s == "" || strings.ContainsAny(s, " \t\n") {
		return r, fmt.Errorf("%w: reference %q", ErrInvalid, s)
	}
	rest := s
	if i := strings.Index(rest, "@"); i >= 0 {
		r.Digest, rest = rest[i+1:], rest[:i]
		if !validDigest(r.Digest) {
			return r, fmt.Errorf("%w: digest %q (want sha256:<64 hex>)", ErrInvalid, r.Digest)
		}
	}
	host := ""
	if i := strings.Index(rest, "/"); i >= 0 {
		first := rest[:i]
		if strings.ContainsAny(first, ".:") || first == "localhost" {
			host, rest = first, rest[i+1:]
		}
	}
	// a tag is after the last colon of the path part
	if i := strings.LastIndex(rest, ":"); i >= 0 && !strings.Contains(rest[i:], "/") {
		r.Tag, rest = rest[i+1:], rest[:i]
	}
	if r.Tag == "" && r.Digest == "" {
		r.Tag = "latest"
	}
	if host == "" || host == "docker.io" || host == "index.docker.io" {
		host = "registry-1.docker.io"
		if !strings.Contains(rest, "/") {
			rest = "library/" + rest
		}
	}
	if rest == "" || rest != strings.ToLower(rest) || strings.Contains(rest, "//") {
		return r, fmt.Errorf("%w: repository %q", ErrInvalid, rest)
	}
	r.Host, r.Repository = host, rest
	return r, nil
}

func validDigest(d string) bool {
	h, ok := strings.CutPrefix(d, "sha256:")
	if !ok || len(h) != 64 {
		return false
	}
	_, err := hex.DecodeString(h)
	return err == nil
}

// Auth is a registry login, used for one pull and never stored. Empty means
// anonymous.
type Auth struct {
	Username, Password string
}

// Puller downloads images.
type Puller struct {
	HTTP *http.Client
	// Insecure talks plain HTTP to the registry (a registry on localhost or a
	// private network without TLS). Never the default.
	Insecure bool
	// Platform to pick from a multi-platform image; empty: linux/<GOARCH>.
	OS, Arch string
}

// Media types the puller understands.
const (
	mtOCIIndex       = "application/vnd.oci.image.index.v1+json"
	mtOCIManifest    = "application/vnd.oci.image.manifest.v1+json"
	mtDockerList     = "application/vnd.docker.distribution.manifest.list.v2+json"
	mtDockerManifest = "application/vnd.docker.distribution.manifest.v2+json"
)

// Descriptor points at content by digest.
type Descriptor struct {
	MediaType string    `json:"mediaType"`
	Digest    string    `json:"digest"`
	Size      int64     `json:"size"`
	Platform  *Platform `json:"platform,omitempty"`
}

type Platform struct {
	OS           string `json:"os"`
	Architecture string `json:"architecture"`
	Variant      string `json:"variant,omitempty"`
}

// manifest is an image manifest or an index (manifest list); which one is in
// MediaType.
type manifest struct {
	SchemaVersion int          `json:"schemaVersion"`
	MediaType     string       `json:"mediaType"`
	Config        Descriptor   `json:"config"`
	Layers        []Descriptor `json:"layers"`
	Manifests     []Descriptor `json:"manifests"`
}

// Image is a resolved image: its manifest digest and layers, bottom first.
type Image struct {
	Ref    Reference
	Digest string
	Layers []Descriptor
}

const maxManifest = 4 << 20

// Resolve finds the image for this platform: an index is narrowed to the
// manifest for linux/<arch>.
func (p *Puller) Resolve(ctx context.Context, ref Reference, auth Auth) (*Image, error) {
	s := &session{p: p, ref: ref, auth: auth}
	target := ref.Digest
	if target == "" {
		target = ref.Tag
	}
	for depth := 0; depth < 2; depth++ {
		data, mt, digest, err := s.manifest(ctx, target)
		if err != nil {
			return nil, err
		}
		var m manifest
		if err := json.Unmarshal(data, &m); err != nil {
			return nil, fmt.Errorf("%w: manifest is not JSON: %v", ErrInvalid, err)
		}
		if m.MediaType != "" {
			mt = m.MediaType
		}
		switch mt {
		case mtOCIIndex, mtDockerList:
			d, err := p.pick(m.Manifests)
			if err != nil {
				return nil, err
			}
			target = d.Digest
			continue
		case mtOCIManifest, mtDockerManifest, "":
			if len(m.Layers) == 0 && len(m.Manifests) > 0 {
				d, err := p.pick(m.Manifests)
				if err != nil {
					return nil, err
				}
				target = d.Digest
				continue
			}
			for _, l := range m.Layers {
				if !validDigest(l.Digest) {
					return nil, fmt.Errorf("%w: layer digest %q", ErrInvalid, l.Digest)
				}
				if _, err := layerCompression(l.MediaType); err != nil {
					return nil, err
				}
			}
			return &Image{Ref: ref, Digest: digest, Layers: m.Layers}, nil
		default:
			return nil, fmt.Errorf("%w: unsupported manifest type %q (Docker schema 1 images are not supported)", ErrInvalid, mt)
		}
	}
	return nil, fmt.Errorf("%w: index points at another index", ErrInvalid)
}

func (p *Puller) pick(list []Descriptor) (Descriptor, error) {
	wantOS, wantArch := p.OS, p.Arch
	if wantOS == "" {
		wantOS = "linux"
	}
	if wantArch == "" {
		wantArch = runtime.GOARCH
	}
	var have []string
	for _, d := range list {
		if d.Platform == nil {
			continue
		}
		if d.Platform.OS == wantOS && d.Platform.Architecture == wantArch {
			return d, nil
		}
		have = append(have, d.Platform.OS+"/"+d.Platform.Architecture)
	}
	return Descriptor{}, fmt.Errorf("%w: no %s/%s image (the image has: %s)", ErrInvalid, wantOS, wantArch, strings.Join(have, ", "))
}

// Layer opens one layer's blob. The reader checks the digest as it is read:
// reading to the end returns ErrCorrupt instead of io.EOF if it does not match.
func (p *Puller) Layer(ctx context.Context, img *Image, d Descriptor, auth Auth) (io.ReadCloser, error) {
	s := &session{p: p, ref: img.Ref, auth: auth}
	resp, err := s.get(ctx, "/blobs/"+d.Digest, "")
	if err != nil {
		return nil, err
	}
	return &verifier{rc: resp.Body, h: sha256.New(), want: d.Digest}, nil
}

// session is one pull: it keeps the token the registry handed out.
type session struct {
	p     *Puller
	ref   Reference
	auth  Auth
	token string
	basic bool
}

func (s *session) client() *http.Client {
	if s.p.HTTP != nil {
		return s.p.HTTP
	}
	return http.DefaultClient
}

func (s *session) base() string {
	scheme := "https"
	if s.p.Insecure {
		scheme = "http"
	}
	return scheme + "://" + s.ref.Host + "/v2/" + s.ref.Repository
}

func (s *session) manifest(ctx context.Context, target string) ([]byte, string, string, error) {
	accept := strings.Join([]string{mtOCIIndex, mtDockerList, mtOCIManifest, mtDockerManifest}, ", ")
	resp, err := s.get(ctx, "/manifests/"+target, accept)
	if err != nil {
		return nil, "", "", err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxManifest+1))
	if err != nil {
		return nil, "", "", err
	}
	if len(data) > maxManifest {
		return nil, "", "", fmt.Errorf("%w: manifest larger than %d bytes", ErrInvalid, maxManifest)
	}
	sum := sha256.Sum256(data)
	digest := "sha256:" + hex.EncodeToString(sum[:])
	if strings.HasPrefix(target, "sha256:") && target != digest {
		return nil, "", "", fmt.Errorf("%w: manifest %s has digest %s", ErrCorrupt, target, digest)
	}
	mt, _, _ := strings.Cut(resp.Header.Get("Content-Type"), ";")
	return data, strings.TrimSpace(mt), digest, nil
}

// get sends a GET, answering one authentication challenge (Bearer token or
// Basic) if the registry asks for it.
func (s *session) get(ctx context.Context, path, accept string) (*http.Response, error) {
	for attempt := 0; attempt < 2; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.base()+path, nil)
		if err != nil {
			return nil, err
		}
		if accept != "" {
			req.Header.Set("Accept", accept)
		}
		switch {
		case s.token != "":
			req.Header.Set("Authorization", "Bearer "+s.token)
		case s.basic:
			req.SetBasicAuth(s.auth.Username, s.auth.Password)
		}
		resp, err := s.client().Do(req)
		if err != nil {
			return nil, fmt.Errorf("registry %s: %w", s.ref.Host, err)
		}
		switch {
		case resp.StatusCode == http.StatusOK:
			return resp, nil
		case resp.StatusCode == http.StatusUnauthorized && attempt == 0:
			challenge := resp.Header.Get("WWW-Authenticate")
			drain(resp)
			if err := s.login(ctx, challenge); err != nil {
				return nil, err
			}
			continue
		case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
			drain(resp)
			return nil, fmt.Errorf("%w: %s (%d)", ErrDenied, s.ref, resp.StatusCode)
		case resp.StatusCode == http.StatusNotFound:
			drain(resp)
			return nil, fmt.Errorf("%w: %s", ErrNotFound, s.ref)
		default:
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
			resp.Body.Close()
			return nil, fmt.Errorf("registry %s answered %d: %s", s.ref.Host, resp.StatusCode, strings.TrimSpace(string(body)))
		}
	}
	return nil, fmt.Errorf("%w: %s", ErrDenied, s.ref)
}

// login answers a WWW-Authenticate challenge.
func (s *session) login(ctx context.Context, challenge string) error {
	scheme, params := parseChallenge(challenge)
	switch strings.ToLower(scheme) {
	case "basic":
		if s.auth.Username == "" {
			return fmt.Errorf("%w: %s needs a username and password", ErrDenied, s.ref.Host)
		}
		s.basic = true
		return nil
	case "bearer":
		realm := params["realm"]
		u, err := url.Parse(realm)
		if err != nil || realm == "" {
			return fmt.Errorf("registry %s: bad token realm %q", s.ref.Host, realm)
		}
		q := u.Query()
		if svc := params["service"]; svc != "" {
			q.Set("service", svc)
		}
		q.Set("scope", "repository:"+s.ref.Repository+":pull")
		u.RawQuery = q.Encode()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
		if err != nil {
			return err
		}
		if s.auth.Username != "" {
			req.SetBasicAuth(s.auth.Username, s.auth.Password)
		}
		resp, err := s.client().Do(req)
		if err != nil {
			return fmt.Errorf("registry %s token: %w", s.ref.Host, err)
		}
		defer resp.Body.Close()
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			return fmt.Errorf("%w: %s refused the credentials", ErrDenied, s.ref.Host)
		}
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("registry %s token: status %d", s.ref.Host, resp.StatusCode)
		}
		var tok struct {
			Token       string `json:"token"`
			AccessToken string `json:"access_token"`
		}
		if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&tok); err != nil {
			return fmt.Errorf("registry %s token: %v", s.ref.Host, err)
		}
		s.token = tok.Token
		if s.token == "" {
			s.token = tok.AccessToken
		}
		if s.token == "" {
			return fmt.Errorf("registry %s handed out an empty token", s.ref.Host)
		}
		return nil
	default:
		return fmt.Errorf("%w: %s asks for unsupported authentication %q", ErrDenied, s.ref.Host, scheme)
	}
}

// parseChallenge splits `Bearer realm="...",service="..."`.
func parseChallenge(h string) (string, map[string]string) {
	scheme, rest, _ := strings.Cut(strings.TrimSpace(h), " ")
	params := map[string]string{}
	for rest != "" {
		rest = strings.TrimLeft(rest, " ,")
		k, v, ok := strings.Cut(rest, "=")
		if !ok {
			break
		}
		k = strings.TrimSpace(k)
		if strings.HasPrefix(v, `"`) {
			end := strings.Index(v[1:], `"`)
			if end < 0 {
				params[k] = v[1:]
				break
			}
			params[k], rest = v[1:end+1], v[end+2:]
		} else {
			val, after, _ := strings.Cut(v, ",")
			params[k], rest = strings.TrimSpace(val), after
		}
	}
	return scheme, params
}

func drain(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	resp.Body.Close()
}

// verifier hashes a blob as it is read and fails the final read on a mismatch.
type verifier struct {
	rc   io.ReadCloser
	h    hash.Hash
	want string
	done bool
}

func (v *verifier) Read(p []byte) (int, error) {
	n, err := v.rc.Read(p)
	v.h.Write(p[:n])
	if errors.Is(err, io.EOF) && !v.done {
		v.done = true
		if got := "sha256:" + hex.EncodeToString(v.h.Sum(nil)); got != v.want {
			return n, fmt.Errorf("%w: blob %s has digest %s", ErrCorrupt, v.want, got)
		}
	}
	return n, err
}

func (v *verifier) Close() error { return v.rc.Close() }
