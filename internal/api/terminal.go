package api

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/AQLD/cairn/internal/engine"
	"github.com/AQLD/cairn/internal/ws"
)

// GET /v1/workspaces/{id}/terminal?rows=R&cols=C[&arg=…]: an interactive
// terminal over a WebSocket, a login shell unless args are given.
//
// Client to server: binary messages are what is typed; text messages are
// JSON controls, {"type":"resize","rows":R,"cols":C}. Server to client:
// binary messages are the terminal's output; the last is a text message
// {"type":"exit","code":N} when the command ends. Closing the WebSocket
// ends the command, as closing a terminal would.
func (s *Server) terminal(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	size := engine.TTYSize{Rows: dimension(q.Get("rows"), 24), Cols: dimension(q.Get("cols"), 80)}
	if !ws.IsUpgrade(r) {
		writeError(w, http.StatusBadRequest, "invalid_argument", "a terminal is a WebSocket: send Upgrade: websocket")
		return
	}
	// refuse before upgrading (unknown workspace, runtime without terminals),
	// so the client gets an ordinary API error
	if !s.eng.Info().Capabilities.Terminal {
		writeError(w, http.StatusNotImplemented, "unsupported_by_runtime", "runtime "+s.eng.Info().Runtime+" has no terminals")
		return
	}
	if _, err := s.eng.GetWorkspace(r.Context(), r.PathValue("id")); err != nil {
		s.fail(w, err)
		return
	}
	conn, err := ws.Upgrade(w, r)
	if err != nil {
		return
	}
	defer conn.Close()
	out := &wsWriter{c: conn}
	term, err := s.eng.OpenTerminal(r.Context(), r.PathValue("id"), size, q["arg"], out)
	if err != nil {
		_ = conn.WriteMessage(ws.Text, mustJSON(map[string]any{"type": "error", "message": err.Error()}))
		return
	}
	defer term.Close()
	go func() {
		for {
			typ, msg, err := conn.ReadMessage()
			if err != nil {
				term.Close() // the person closed the terminal
				return
			}
			switch typ {
			case ws.Binary:
				if _, err := term.Write(msg); err != nil {
					return
				}
			case ws.Text:
				var ctl struct {
					Type       string
					Rows, Cols uint16
				}
				if json.Unmarshal(msg, &ctl) == nil && ctl.Type == "resize" && ctl.Rows > 0 && ctl.Cols > 0 {
					_ = term.Resize(engine.TTYSize{Rows: ctl.Rows, Cols: ctl.Cols})
				}
			}
		}
	}()
	select {
	case <-term.Done():
	case <-s.stopping():
		term.Close()
		<-term.Done()
	}
	code, err := term.Result()
	msg := map[string]any{"type": "exit", "code": code}
	if err != nil {
		msg["error"] = err.Error()
	}
	_ = conn.WriteMessage(ws.Text, mustJSON(msg))
}

func dimension(v string, def uint16) uint16 {
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 || n > 1000 {
		return def
	}
	return uint16(n)
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

// wsWriter sends what the terminal writes as binary messages.
type wsWriter struct{ c *ws.Conn }

func (w *wsWriter) Write(p []byte) (int, error) {
	if err := w.c.WriteMessage(ws.Binary, p); err != nil {
		return 0, err
	}
	return len(p), nil
}
