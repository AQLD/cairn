package client

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"sync"

	"github.com/AQLD/cairn/internal/ws"
)

// Terminal is an interactive terminal in a workspace (OpenTerminal).
type Terminal struct {
	conn *ws.Conn
	done chan struct{}
	mu   sync.Mutex
	code int
	err  error
}

// OpenTerminal starts a login shell (or args) on a terminal of rows x cols in
// a workspace and copies its output to out until it ends. Write types into
// it, Resize follows the window, Close ends it; Wait returns its exit code.
func (c *Client) OpenTerminal(ctx context.Context, id string, rows, cols int, out io.Writer, args ...string) (*Terminal, error) {
	v := url.Values{"rows": {strconv.Itoa(rows)}, "cols": {strconv.Itoa(cols)}}
	for _, a := range args {
		v.Add("arg", a)
	}
	h := http.Header{}
	if c.token != "" {
		h.Set("Authorization", "Bearer "+c.token)
	}
	var tlsConfig *tls.Config
	if tr, ok := c.http.Transport.(*http.Transport); ok && tr.TLSClientConfig != nil {
		tlsConfig = tr.TLSClientConfig
	}
	conn, err := ws.Dial(ctx, c.base+"/v1/workspaces/"+url.PathEscape(id)+"/terminal?"+v.Encode(), h, tlsConfig)
	var he *ws.HandshakeError
	if errors.As(err, &he) {
		defer he.Response.Body.Close()
		return nil, readError(he.Response)
	}
	if err != nil {
		return nil, err
	}
	t := &Terminal{conn: conn, done: make(chan struct{})}
	go func() {
		defer close(t.done)
		for {
			typ, msg, err := conn.ReadMessage()
			if err != nil {
				t.finish(-1, errors.New("the terminal's connection ended"))
				return
			}
			if typ == ws.Binary {
				if _, err := out.Write(msg); err != nil {
					t.finish(-1, err)
					return
				}
				continue
			}
			var ctl struct {
				Type    string `json:"type"`
				Code    int    `json:"code"`
				Error   string `json:"error"`
				Message string `json:"message"`
			}
			if json.Unmarshal(msg, &ctl) != nil {
				continue
			}
			switch ctl.Type {
			case "exit":
				var err error
				if ctl.Error != "" {
					err = errors.New(ctl.Error)
				}
				t.finish(ctl.Code, err)
				return
			case "error":
				t.finish(-1, errors.New(ctl.Message))
				return
			}
		}
	}()
	return t, nil
}

func (t *Terminal) finish(code int, err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.code, t.err = code, err
}

// Write types p into the terminal.
func (t *Terminal) Write(p []byte) (int, error) {
	if err := t.conn.WriteMessage(ws.Binary, p); err != nil {
		return 0, err
	}
	return len(p), nil
}

// Resize tells the terminal its new size.
func (t *Terminal) Resize(rows, cols int) error {
	b, _ := json.Marshal(map[string]any{"type": "resize", "rows": rows, "cols": cols})
	return t.conn.WriteMessage(ws.Text, b)
}

// Close ends the terminal's command.
func (t *Terminal) Close() error { return t.conn.Close() }

// Done is closed when the terminal has ended.
func (t *Terminal) Done() <-chan struct{} { return t.done }

// Wait waits for the command to end and returns its exit code.
func (t *Terminal) Wait() (int, error) {
	<-t.done
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.code, t.err
}
