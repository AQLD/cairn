// Package s3test is an in-memory S3 server for tests: objects, multipart
// uploads and ListObjectsV2, plus fault injection (dropped connections). It
// does not check signatures; test against a real S3-compatible server (MinIO)
// for that.
package s3test

import (
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
)

type Server struct {
	*httptest.Server

	mu      sync.Mutex
	objects map[string][]byte // "bucket/key"
	uploads map[string]map[int][]byte
	next    int

	// Fail, if set, is asked before each request; true drops the connection
	// (like a network failure) instead of answering.
	Fail func(r *http.Request) bool
	// Requests counts the requests received.
	Requests int
}

func New() *Server {
	s := &Server{objects: map[string][]byte{}, uploads: map[string]map[int][]byte{}}
	s.Server = httptest.NewServer(http.HandlerFunc(s.serve))
	return s
}

// Object returns a stored object's content.
func (s *Server) Object(bucketKey string) ([]byte, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.objects[bucketKey]
	return b, ok
}

// SetObject replaces an object's content (to simulate damage in storage).
func (s *Server) SetObject(bucketKey string, b []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.objects[bucketKey] = b
}

// Keys lists stored objects ("bucket/key"), sorted.
func (s *Server) Keys() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	keys := make([]string, 0, len(s.objects))
	for k := range s.objects {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.Requests++
	fail := s.Fail
	s.mu.Unlock()
	if fail != nil && fail(r) {
		if hj, ok := w.(http.Hijacker); ok {
			if conn, _, err := hj.Hijack(); err == nil {
				conn.Close()
				return
			}
		}
		http.Error(w, "injected failure", http.StatusServiceUnavailable)
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/")
	bucket, key, _ := strings.Cut(path, "/")
	q := r.URL.Query()
	full := bucket + "/" + key

	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case r.Method == http.MethodGet && key == "" && q.Get("list-type") == "2":
		prefix := bucket + "/" + q.Get("prefix")
		type content struct {
			Key string `xml:"Key"`
		}
		var out struct {
			XMLName     xml.Name  `xml:"ListBucketResult"`
			Contents    []content `xml:"Contents"`
			IsTruncated bool      `xml:"IsTruncated"`
		}
		var keys []string
		for k := range s.objects {
			if strings.HasPrefix(k, prefix) {
				keys = append(keys, strings.TrimPrefix(k, bucket+"/"))
			}
		}
		sort.Strings(keys)
		for _, k := range keys {
			out.Contents = append(out.Contents, content{Key: k})
		}
		writeXML(w, out)
	case r.Method == http.MethodPost && q.Has("uploads"):
		s.next++
		id := strconv.Itoa(s.next)
		s.uploads[id] = map[int][]byte{}
		writeXML(w, struct {
			XMLName  xml.Name `xml:"InitiateMultipartUploadResult"`
			UploadID string   `xml:"UploadId"`
		}{UploadID: id})
	case r.Method == http.MethodPut && q.Has("uploadId"):
		parts, ok := s.uploads[q.Get("uploadId")]
		if !ok {
			http.Error(w, "<Error><Code>NoSuchUpload</Code></Error>", http.StatusNotFound)
			return
		}
		n, _ := strconv.Atoi(q.Get("partNumber"))
		b, err := io.ReadAll(r.Body)
		if err != nil {
			return
		}
		parts[n] = b
		w.Header().Set("ETag", fmt.Sprintf("%q", fmt.Sprintf("etag-%d", n)))
	case r.Method == http.MethodPost && q.Has("uploadId"):
		parts, ok := s.uploads[q.Get("uploadId")]
		if !ok {
			http.Error(w, "<Error><Code>NoSuchUpload</Code></Error>", http.StatusNotFound)
			return
		}
		var all []byte
		for i := 1; i <= len(parts); i++ {
			all = append(all, parts[i]...)
		}
		s.objects[full] = all
		delete(s.uploads, q.Get("uploadId"))
		writeXML(w, struct {
			XMLName xml.Name `xml:"CompleteMultipartUploadResult"`
		}{})
	case r.Method == http.MethodDelete && q.Has("uploadId"):
		delete(s.uploads, q.Get("uploadId"))
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodPut:
		b, err := io.ReadAll(r.Body)
		if err != nil {
			return
		}
		s.objects[full] = b
	case r.Method == http.MethodGet:
		b, ok := s.objects[full]
		if !ok {
			http.Error(w, "<Error><Code>NoSuchKey</Code></Error>", http.StatusNotFound)
			return
		}
		_, _ = w.Write(b)
	case r.Method == http.MethodDelete:
		delete(s.objects, full)
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, "unsupported", http.StatusBadRequest)
	}
}

func writeXML(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/xml")
	_ = xml.NewEncoder(w).Encode(v)
}
