// Package ws is the part of WebSocket (RFC 6455) a terminal needs: the
// handshake, binary and text messages, ping/pong and close, for both the
// server (the engine's API) and the client (the CLI). No dependencies.
package ws

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
)

// Message types.
const (
	Text   = 1
	Binary = 2
	close_ = 8
	opPing = 9
	opPong = 10
)

const guid = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

// maxMessage bounds one message (a paste, a burst of output).
const maxMessage = 1 << 20

// Conn is a WebSocket connection. Reads come from one goroutine; writes may
// come from several.
type Conn struct {
	conn   net.Conn
	br     *bufio.Reader
	client bool // a client masks what it sends
	wmu    sync.Mutex
}

func accept(key string) string {
	h := sha1.Sum([]byte(key + guid))
	return base64.StdEncoding.EncodeToString(h[:])
}

// IsUpgrade reports whether a request asks for a WebSocket.
func IsUpgrade(r *http.Request) bool {
	return strings.EqualFold(r.Header.Get("Upgrade"), "websocket") &&
		strings.Contains(strings.ToLower(r.Header.Get("Connection")), "upgrade")
}

// Upgrade answers a WebSocket handshake and takes over the connection.
func Upgrade(w http.ResponseWriter, r *http.Request) (*Conn, error) {
	key := r.Header.Get("Sec-WebSocket-Key")
	if !IsUpgrade(r) || key == "" || r.Header.Get("Sec-WebSocket-Version") != "13" {
		return nil, errors.New("not a WebSocket request")
	}
	// through any wrapper (logging, metrics) that offers Unwrap
	conn, brw, err := http.NewResponseController(w).Hijack()
	if err != nil {
		return nil, fmt.Errorf("the connection cannot be taken over: %w", err)
	}
	resp := "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: " + accept(key) + "\r\n\r\n"
	if _, err := conn.Write([]byte(resp)); err != nil {
		conn.Close()
		return nil, err
	}
	return &Conn{conn: conn, br: brw.Reader}, nil
}

// Dial opens a WebSocket to an http(s) URL with the given headers (a token).
func Dial(ctx context.Context, rawURL string, header http.Header, tlsConfig *tls.Config) (*Conn, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}
	host := u.Host
	if u.Port() == "" {
		if u.Scheme == "https" {
			host += ":443"
		} else {
			host += ":80"
		}
	}
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", host)
	if err != nil {
		return nil, err
	}
	if u.Scheme == "https" {
		cfg := tlsConfig.Clone()
		if cfg == nil {
			cfg = &tls.Config{}
		}
		cfg.ServerName = u.Hostname()
		tc := tls.Client(conn, cfg)
		if err := tc.HandshakeContext(ctx); err != nil {
			conn.Close()
			return nil, err
		}
		conn = tc
	}
	raw := make([]byte, 16)
	_, _ = rand.Read(raw)
	key := base64.StdEncoding.EncodeToString(raw)
	req, _ := http.NewRequest(http.MethodGet, u.String(), nil)
	for k, vs := range header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Sec-WebSocket-Key", key)
	req.Header.Set("Sec-WebSocket-Version", "13")
	if err := req.Write(conn); err != nil {
		conn.Close()
		return nil, err
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		conn.Close()
		return nil, err
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		defer conn.Close()
		return nil, &HandshakeError{Response: resp}
	}
	if resp.Header.Get("Sec-WebSocket-Accept") != accept(key) {
		conn.Close()
		return nil, errors.New("websocket: the server's handshake does not match")
	}
	return &Conn{conn: conn, br: br, client: true}, nil
}

// HandshakeError is a refused handshake: the server's HTTP answer (an API
// error, read it with the client's usual decoding).
type HandshakeError struct{ Response *http.Response }

func (e *HandshakeError) Error() string {
	return fmt.Sprintf("websocket: refused with %s", e.Response.Status)
}

// WriteMessage sends one message.
func (c *Conn) WriteMessage(typ int, data []byte) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	return c.writeFrame(byte(typ), data)
}

func (c *Conn) writeFrame(op byte, data []byte) error {
	hdr := []byte{0x80 | op, 0}
	n := len(data)
	switch {
	case n < 126:
		hdr[1] = byte(n)
	case n <= 0xffff:
		hdr[1] = 126
		hdr = binary.BigEndian.AppendUint16(hdr, uint16(n))
	default:
		hdr[1] = 127
		hdr = binary.BigEndian.AppendUint64(hdr, uint64(n))
	}
	payload := data
	if c.client {
		hdr[1] |= 0x80
		mask := make([]byte, 4)
		_, _ = rand.Read(mask)
		hdr = append(hdr, mask...)
		payload = make([]byte, n)
		for i := range data {
			payload[i] = data[i] ^ mask[i%4]
		}
	}
	_, err := c.conn.Write(append(hdr, payload...))
	return err
}

// ReadMessage returns the next text or binary message, answering pings on
// the way. A close from the other side is io.EOF.
func (c *Conn) ReadMessage() (int, []byte, error) {
	var typ int
	var msg []byte
	for {
		hdr := make([]byte, 2)
		if _, err := io.ReadFull(c.br, hdr); err != nil {
			return 0, nil, err
		}
		fin, op := hdr[0]&0x80 != 0, hdr[0]&0x0f
		masked := hdr[1]&0x80 != 0
		n := uint64(hdr[1] & 0x7f)
		switch n {
		case 126:
			b := make([]byte, 2)
			if _, err := io.ReadFull(c.br, b); err != nil {
				return 0, nil, err
			}
			n = uint64(binary.BigEndian.Uint16(b))
		case 127:
			b := make([]byte, 8)
			if _, err := io.ReadFull(c.br, b); err != nil {
				return 0, nil, err
			}
			n = binary.BigEndian.Uint64(b)
		}
		if n > maxMessage || uint64(len(msg))+n > maxMessage {
			return 0, nil, errors.New("websocket: message too large")
		}
		if masked == c.client { // a client never receives masked frames; a server only masked ones
			return 0, nil, errors.New("websocket: wrong masking")
		}
		var mask []byte
		if masked {
			mask = make([]byte, 4)
			if _, err := io.ReadFull(c.br, mask); err != nil {
				return 0, nil, err
			}
		}
		payload := make([]byte, n)
		if _, err := io.ReadFull(c.br, payload); err != nil {
			return 0, nil, err
		}
		for i := range payload {
			if mask != nil {
				payload[i] ^= mask[i%4]
			}
		}
		switch op {
		case opPing:
			c.wmu.Lock()
			err := c.writeFrame(opPong, payload)
			c.wmu.Unlock()
			if err != nil {
				return 0, nil, err
			}
			continue
		case opPong:
			continue
		case close_:
			c.wmu.Lock()
			_ = c.writeFrame(close_, payload)
			c.wmu.Unlock()
			return 0, nil, io.EOF
		case 0: // continuation
		default:
			typ = int(op)
		}
		msg = append(msg, payload...)
		if fin {
			return typ, msg, nil
		}
	}
}

// Close says goodbye and closes the connection.
func (c *Conn) Close() error {
	c.wmu.Lock()
	_ = c.writeFrame(close_, []byte{0x03, 0xe8}) // 1000: normal
	c.wmu.Unlock()
	return c.conn.Close()
}
