package acceptance

import (
	"bufio"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/AQLD/cairn/client"
)

// G10.4: metrics and structured logs cover every operation.

func init() {
	register("G10.4.1", func(e *Env) {
		ws := e.Workspace("metrics")
		e.Write(ws.ID, "f", "x")
		sv := e.Save(ws.ID)
		e.Resume(sv.ID)
		_, _ = e.C.GetWorkspace(e.Ctx, "ws_missing")

		text, err := e.C.Metrics(e.Ctx)
		e.Must(err)
		for _, want := range []string{
			`cairn_http_requests_total{route="POST /v1/workspaces",code="201"} 1`,
			`cairn_http_requests_total{route="POST /v1/workspaces/{id}/saves",code="201"} 1`,
			`cairn_http_requests_total{route="POST /v1/saves/{id}/resume",code="200"} 1`,
			`cairn_http_requests_total{route="GET /v1/workspaces/{id}",code="404"} 1`,
			`cairn_http_request_duration_seconds_count{route="POST /v1/saves/{id}/resume"} 1`,
			`cairn_workspaces{state="running"} 1`,
			`cairn_saves{state="ready"} 1`,
		} {
			if !strings.Contains(text, want) {
				e.T.Fatalf("metrics missing %q\n%s", want, text)
			}
		}
		if strings.Contains(text, ws.ID) || strings.Contains(text, sv.ID) {
			e.T.Fatal("metrics labels contain workspace or save IDs (unbounded cardinality)")
		}
	})

	register("G10.4.2", func(e *Env) {
		// a request ID the client chose is echoed and logged
		req, err := http.NewRequestWithContext(e.Ctx, http.MethodGet, e.srv.URL+"/v1/workspaces/ws_missing", nil)
		e.Must(err)
		req.Header.Set("X-Request-Id", "trace-abc-123")
		resp, err := http.DefaultClient.Do(req)
		e.Must(err)
		resp.Body.Close()
		Equal(e, "echoed request id", resp.Header.Get("X-Request-Id"), "trace-abc-123")

		// without one, the engine makes one, and client errors carry it
		_, err = e.C.GetWorkspace(e.Ctx, "ws_missing_too")
		var apiErr *client.Error
		if ce, ok := err.(*client.Error); ok {
			apiErr = ce
		}
		if apiErr == nil || apiErr.RequestID == "" {
			e.T.Fatalf("client error carries no request id: %v", err)
		}

		found := map[string]bool{}
		sc := bufio.NewScanner(strings.NewReader(e.Logs.String()))
		for sc.Scan() {
			var line map[string]any
			if json.Unmarshal(sc.Bytes(), &line) != nil || line["msg"] != "request" {
				continue
			}
			for _, field := range []string{"request_id", "method", "route", "status", "duration_ms"} {
				if _, ok := line[field]; !ok {
					e.T.Fatalf("log line missing %s: %s", field, sc.Text())
				}
			}
			found[line["request_id"].(string)] = true
		}
		if !found["trace-abc-123"] || !found[apiErr.RequestID] {
			e.T.Fatalf("requests not found in the log by id; log:\n%s", e.Logs.String())
		}
	})
}
