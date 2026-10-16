package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"time"

	"github.com/AQLD/cairn/internal/metrics"
)

// RequestIDHeader carries a request's ID: taken from the client if it sent a
// sensible one, generated otherwise, and always echoed back and logged, so a
// client-side error can be matched to the engine's log line.
const RequestIDHeader = "X-Request-Id"

var validRequestID = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

type ctxKey struct{}

// RequestID returns the ID of the request ctx belongs to.
func RequestID(ctx context.Context) string {
	id, _ := ctx.Value(ctxKey{}).(string)
	return id
}

// observe wraps every request: request ID, one structured log line, and
// per-route metrics. The route is the mux pattern, so IDs never become labels.
func (s *Server) observe(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		id := r.Header.Get(RequestIDHeader)
		if !validRequestID.MatchString(id) {
			var b [8]byte
			_, _ = rand.Read(b[:])
			id = hex.EncodeToString(b[:])
		}
		w.Header().Set(RequestIDHeader, id)
		r = r.WithContext(context.WithValue(r.Context(), ctxKey{}, id))
		rec := &recorder{ResponseWriter: w, status: http.StatusOK}

		next.ServeHTTP(rec, r)

		route := r.Pattern
		if route == "" {
			route = "unmatched"
		}
		elapsed := time.Since(start)
		code := strconv.Itoa(rec.status)
		s.metrics.Counter("cairn_http_requests_total", "API requests by route and status code.",
			[]string{"route", "code"}, []string{route, code}, 1)
		s.metrics.Observe("cairn_http_request_duration_seconds", "API request latency by route.",
			[]string{"route"}, []string{route}, elapsed.Seconds())

		attrs := []any{
			"request_id", id, "method", r.Method, "route", route, "status", rec.status,
			"duration_ms", elapsed.Milliseconds(), "bytes", rec.bytes,
		}
		if v := r.PathValue("id"); v != "" {
			attrs = append(attrs, "target", v)
		}
		level := slog.LevelInfo
		if rec.status >= 500 {
			level = slog.LevelError
		}
		s.log.Log(r.Context(), level, "request", attrs...)
	})
}

// recorder captures the status and size of a response. Unwrap lets
// http.ResponseController reach the real writer, so streaming still flushes.
type recorder struct {
	http.ResponseWriter
	status      int
	bytes       int64
	wroteHeader bool
}

func (r *recorder) WriteHeader(code int) {
	if !r.wroteHeader {
		r.status, r.wroteHeader = code, true
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *recorder) Write(p []byte) (int, error) {
	r.wroteHeader = true
	n, err := r.ResponseWriter.Write(p)
	r.bytes += int64(n)
	return n, err
}

func (r *recorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// registerGauges exposes the engine's live counts on every scrape.
func (s *Server) registerGauges() {
	s.metrics.Gauge("cairn_workspaces", "Workspaces by state.", func() []metrics.Sample {
		c, err := s.eng.Counts(context.Background())
		if err != nil {
			return nil
		}
		var out []metrics.Sample
		for st, n := range c.Workspaces {
			out = append(out, metrics.Sample{Labels: map[string]string{"state": string(st)}, Value: float64(n)})
		}
		return out
	})
	s.metrics.Gauge("cairn_saves", "Saves by state.", func() []metrics.Sample {
		c, err := s.eng.Counts(context.Background())
		if err != nil {
			return nil
		}
		var out []metrics.Sample
		for st, n := range c.Saves {
			out = append(out, metrics.Sample{Labels: map[string]string{"state": string(st)}, Value: float64(n)})
		}
		return out
	})
	s.metrics.Gauge("cairn_save_bytes", "Bytes held by saves that have not been purged.", func() []metrics.Sample {
		c, err := s.eng.Counts(context.Background())
		if err != nil {
			return nil
		}
		return []metrics.Sample{{Value: float64(c.SaveBytes)}}
	})
	// workspace disks are sparse: alert on this before it reaches zero
	s.metrics.Gauge("cairn_volume_free_bytes", "Free space on the volume holding workspaces and saves.", func() []metrics.Sample {
		free, err := s.eng.VolumeFree()
		if err != nil {
			return nil
		}
		return []metrics.Sample{{Value: float64(free)}}
	})
}
