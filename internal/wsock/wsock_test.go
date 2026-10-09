package wsock

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestAcceptKey(t *testing.T) {
	// The example of RFC 6455 §1.3.
	if got := AcceptKey("dGhlIHNhbXBsZSBub25jZQ=="); got != "s3pPLMBiTxaQ9kYGzzhZRbK+xOo=" {
		t.Fatalf("AcceptKey = %q", got)
	}
}

func TestUpgradeRejectsBadHandshakes(t *testing.T) {
	good := func() *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/ws", nil)
		r.Header.Set("Connection", "keep-alive, Upgrade")
		r.Header.Set("Upgrade", "websocket")
		r.Header.Set("Sec-WebSocket-Version", "13")
		r.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
		return r
	}
	cases := []struct {
		name   string
		edit   func(*http.Request)
		status int
	}{
		{"post", func(r *http.Request) { r.Method = http.MethodPost }, http.StatusMethodNotAllowed},
		{"no upgrade", func(r *http.Request) { r.Header.Del("Upgrade") }, http.StatusBadRequest},
		{"no connection token", func(r *http.Request) { r.Header.Set("Connection", "keep-alive") }, http.StatusBadRequest},
		{"version", func(r *http.Request) { r.Header.Set("Sec-WebSocket-Version", "8") }, http.StatusUpgradeRequired},
		{"short key", func(r *http.Request) { r.Header.Set("Sec-WebSocket-Key", "c2hvcnQ=") }, http.StatusBadRequest},
		{"bad key", func(r *http.Request) { r.Header.Set("Sec-WebSocket-Key", "not base64!") }, http.StatusBadRequest},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := good()
			c.edit(r)
			w := httptest.NewRecorder()
			_, err := Upgrade(w, r, nil)
			var he *HandshakeError
			if !errors.As(err, &he) || w.Code != c.status || he.Status != c.status {
				t.Fatalf("err %v, status %d, want %d", err, w.Code, c.status)
			}
			if c.status == http.StatusUpgradeRequired && w.Header().Get("Sec-WebSocket-Version") != "13" {
				t.Fatal("426 without Sec-WebSocket-Version")
			}
		})
	}
}

// echoServer upgrades and echoes every message until the peer closes.
func echoServer(t *testing.T, done chan<- error) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := Upgrade(w, r, http.Header{"X-Test": {"yes"}})
		if err != nil {
			return
		}
		defer c.Close()
		for {
			op, data, err := c.ReadMessage()
			if err != nil {
				done <- err
				return
			}
			if err := c.WriteMessage(op, data); err != nil {
				done <- err
				return
			}
		}
	}))
}

func TestDialEchoAndClose(t *testing.T) {
	done := make(chan error, 1)
	srv := echoServer(t, done)
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, resp, err := Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+"/x", http.Header{"Origin": {srv.URL}})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if resp.Header.Get("X-Test") != "yes" {
		t.Fatal("extra response header missing")
	}
	// Short, 16-bit and 64-bit length encodings, both message types.
	for _, msg := range []struct {
		op   int
		data []byte
	}{
		{TextMessage, []byte("merhaba dünya")},
		{BinaryMessage, bytes.Repeat([]byte{0, 1, 2, 255}, 50)},
		{BinaryMessage, bytes.Repeat([]byte("x"), 70000)},
		{TextMessage, []byte{}},
	} {
		if err := c.WriteMessage(msg.op, msg.data); err != nil {
			t.Fatal(err)
		}
		op, data, err := c.ReadMessage()
		if err != nil || op != msg.op || !bytes.Equal(data, msg.data) {
			t.Fatalf("echo: op %d len %d err %v", op, len(data), err)
		}
	}
	if err := c.WriteClose(CloseNormal, "güle güle"); err != nil {
		t.Fatal(err)
	}
	// The server answers the close; the client sees the echo.
	if _, _, err := c.ReadMessage(); !IsClose(err, CloseNormal) {
		t.Fatalf("client read after close: %v", err)
	}
	select {
	case err := <-done:
		var ce *CloseError
		if !errors.As(err, &ce) || ce.Code != CloseNormal || ce.Text != "güle güle" || ce.Local {
			t.Fatalf("server saw %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("server did not see the close")
	}
	if err := c.WriteMessage(TextMessage, []byte("late")); !errors.Is(err, ErrClosed) {
		t.Fatalf("write after close: %v", err)
	}
}

func TestDialRefused(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "forbidden", http.StatusForbidden)
	}))
	defer srv.Close()
	_, resp, err := Dial(context.Background(), "ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	var he *HandshakeError
	if !errors.As(err, &he) || resp == nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("err %v", err)
	}
}

// ---- raw frames against a server Conn ----

// rawFrame builds a frame as a (possibly misbehaving) client would.
func rawFrame(fin bool, rsv byte, op int, mask bool, payload []byte) []byte {
	b := []byte{byte(op) | rsv<<4}
	if fin {
		b[0] |= 0x80
	}
	var m byte
	if mask {
		m = 0x80
	}
	switch l := len(payload); {
	case l <= 125:
		b = append(b, m|byte(l))
	case l <= 0xffff:
		b = append(b, m|126, byte(l>>8), byte(l))
	default:
		var ext [8]byte
		binary.BigEndian.PutUint64(ext[:], uint64(l))
		b = append(append(b, m|127), ext[:]...)
	}
	p := append([]byte(nil), payload...)
	if mask {
		key := [4]byte{0x37, 0xfa, 0x21, 0x3d}
		b = append(b, key[:]...)
		maskBytes(key, 0, p)
	}
	return append(b, p...)
}

func closeFrame(code int, reason string) []byte {
	return rawFrame(true, 0, CloseMessage, true, closePayload(code, reason))
}

type result struct {
	op   int
	data []byte
	err  error
}

// pipe returns a server Conn reading in the background and the raw client
// side.
func pipe(t *testing.T) (*Conn, net.Conn, *bufio.Reader, chan result) {
	t.Helper()
	srvSide, cliSide := net.Pipe()
	c := newConn(srvSide, nil, true)
	c.MaxMessage = 1000
	out := make(chan result, 8)
	go func() {
		for {
			op, data, err := c.ReadMessage()
			out <- result{op, data, err}
			if err != nil {
				return
			}
		}
	}()
	t.Cleanup(func() { srvSide.Close(); cliSide.Close() })
	cliSide.SetDeadline(time.Now().Add(5 * time.Second))
	return c, cliSide, bufio.NewReader(cliSide), out
}

// readServerFrame reads one (unmasked) frame the server sent.
func readServerFrame(t *testing.T, br *bufio.Reader) (op int, payload []byte) {
	t.Helper()
	var h [2]byte
	if _, err := io.ReadFull(br, h[:]); err != nil {
		t.Fatalf("read frame: %v", err)
	}
	if h[1]&0x80 != 0 {
		t.Fatal("server frame is masked")
	}
	n := int(h[1] & 0x7f)
	switch n {
	case 126:
		var b [2]byte
		io.ReadFull(br, b[:])
		n = int(binary.BigEndian.Uint16(b[:]))
	case 127:
		var b [8]byte
		io.ReadFull(br, b[:])
		n = int(binary.BigEndian.Uint64(b[:]))
	}
	payload = make([]byte, n)
	if _, err := io.ReadFull(br, payload); err != nil {
		t.Fatal(err)
	}
	return int(h[0] & 0x0f), payload
}

func expectClose(t *testing.T, br *bufio.Reader, code int) {
	t.Helper()
	op, p := readServerFrame(t, br)
	if op != CloseMessage {
		t.Fatalf("got opcode %d, want close", op)
	}
	if code == CloseNoStatus {
		if len(p) != 0 {
			t.Fatalf("close payload %v, want empty", p)
		}
		return
	}
	if len(p) < 2 || int(binary.BigEndian.Uint16(p)) != code {
		t.Fatalf("close payload %v, want code %d", p, code)
	}
}

func TestFragmentedMaskedMessageWithInterleavedPing(t *testing.T) {
	_, cli, br, out := pipe(t)
	go func() {
		cli.Write(rawFrame(false, 0, TextMessage, true, []byte("mer")))
		cli.Write(rawFrame(true, 0, PingMessage, true, []byte("p1")))
		cli.Write(rawFrame(false, 0, continuationFrame, true, []byte("ha")))
		cli.Write(rawFrame(true, 0, PongMessage, true, []byte("ignored")))
		cli.Write(rawFrame(true, 0, continuationFrame, true, []byte("ba")))
	}()
	// The ping is answered while the message is still incomplete.
	op, p := readServerFrame(t, br)
	if op != PongMessage || string(p) != "p1" {
		t.Fatalf("got %d %q, want pong p1", op, p)
	}
	r := <-out
	if r.err != nil || r.op != TextMessage || string(r.data) != "merhaba" {
		t.Fatalf("got %+v", r)
	}
}

func TestProtocolViolationsClose(t *testing.T) {
	cases := []struct {
		name   string
		frames [][]byte
		code   int
	}{
		{"unmasked client frame", [][]byte{rawFrame(true, 0, TextMessage, false, []byte("hi"))}, CloseProtocolError},
		{"continuation first", [][]byte{rawFrame(true, 0, continuationFrame, true, []byte("x"))}, CloseProtocolError},
		{"new message inside fragmented", [][]byte{
			rawFrame(false, 0, TextMessage, true, []byte("a")),
			rawFrame(true, 0, BinaryMessage, true, []byte("b")),
		}, CloseProtocolError},
		{"fragmented ping", [][]byte{rawFrame(false, 0, PingMessage, true, []byte("x"))}, CloseProtocolError},
		{"long ping", [][]byte{rawFrame(true, 0, PingMessage, true, bytes.Repeat([]byte("x"), 126))}, CloseProtocolError},
		{"reserved bits", [][]byte{rawFrame(true, 4, TextMessage, true, []byte("x"))}, CloseProtocolError},
		{"unknown opcode", [][]byte{rawFrame(true, 0, 3, true, []byte("x"))}, CloseProtocolError},
		{"invalid utf-8", [][]byte{rawFrame(true, 0, TextMessage, true, []byte{0xff, 0xfe})}, CloseInvalidPayload},
		{"too big frame", [][]byte{rawFrame(true, 0, BinaryMessage, true, make([]byte, 1001))}, CloseTooBig},
		{"too big after fragments", [][]byte{
			rawFrame(false, 0, BinaryMessage, true, make([]byte, 600)),
			rawFrame(true, 0, continuationFrame, true, make([]byte, 600)),
		}, CloseTooBig},
		// 1005 and 1006 must never appear in a close frame.
		{"close code 1005 on the wire", [][]byte{rawFrame(true, 0, CloseMessage, true, []byte{0x03, 0xed})}, CloseProtocolError},
		{"close with 1-byte payload", [][]byte{rawFrame(true, 0, CloseMessage, true, []byte{3})}, CloseProtocolError},
		{"close code 999", [][]byte{rawFrame(true, 0, CloseMessage, true, []byte{0x03, 0xe7})}, CloseProtocolError},
		{"close code 1006", [][]byte{rawFrame(true, 0, CloseMessage, true, []byte{0x03, 0xee})}, CloseProtocolError},
		{"close reason not utf-8", [][]byte{rawFrame(true, 0, CloseMessage, true, []byte{0x03, 0xe8, 0xff})}, CloseInvalidPayload},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, cli, br, out := pipe(t)
			go func() {
				for _, f := range c.frames {
					if _, err := cli.Write(f); err != nil {
						return
					}
				}
			}()
			expectClose(t, br, c.code)
			r := <-out
			var ce *CloseError
			if !errors.As(r.err, &ce) || ce.Code != c.code || !ce.Local {
				t.Fatalf("read error %v, want local close %d", r.err, c.code)
			}
		})
	}
}

func TestPeerCloseCodes(t *testing.T) {
	cases := []struct {
		name  string
		frame []byte
		code  int
		text  string
	}{
		{"normal with reason", closeFrame(CloseNormal, "bitti"), CloseNormal, "bitti"},
		{"no status", rawFrame(true, 0, CloseMessage, true, nil), CloseNoStatus, ""},
		{"going away", closeFrame(CloseGoingAway, ""), CloseGoingAway, ""},
		{"application code", closeFrame(4000, "idle"), 4000, "idle"},
		{"registered 3000", closeFrame(3000, ""), 3000, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv, cli, br, out := pipe(t)
			go cli.Write(c.frame)
			// The server echoes the code (an empty close for "no status").
			expectClose(t, br, c.code)
			r := <-out
			var ce *CloseError
			if !errors.As(r.err, &ce) || ce.Code != c.code || ce.Text != c.text || ce.Local {
				t.Fatalf("got %v", r.err)
			}
			// Sticky: later reads return the same close; writes fail.
			if _, _, err := srv.ReadMessage(); !IsClose(err, c.code) {
				t.Fatalf("second read: %v", err)
			}
			if err := srv.WriteMessage(TextMessage, []byte("x")); !errors.Is(err, ErrClosed) {
				t.Fatalf("write after close: %v", err)
			}
		})
	}
}

func TestServerInitiatedClose(t *testing.T) {
	srv, cli, br, out := pipe(t)
	go srv.WriteClose(4001, "oturum süresi doldu")
	op, p := readServerFrame(t, br)
	if op != CloseMessage || binary.BigEndian.Uint16(p) != 4001 || string(p[2:]) != "oturum süresi doldu" {
		t.Fatalf("got %d %q", op, p)
	}
	go cli.Write(closeFrame(4001, ""))
	r := <-out
	if !IsClose(r.err, 4001) {
		t.Fatalf("got %v", r.err)
	}
}

func TestClosePayloadTrimsReason(t *testing.T) {
	p := closePayload(CloseNormal, strings.Repeat("ş", 100)) // 200 bytes
	if len(p) > maxControlPayload {
		t.Fatalf("payload %d bytes", len(p))
	}
	if !bytes.HasPrefix(p, []byte{0x03, 0xe8}) || !strings.HasPrefix(string(p[2:]), "şş") ||
		strings.ContainsRune(string(p[2:]), 0xfffd) {
		t.Fatalf("bad payload %q", p)
	}
	if err := (&Conn{}).WriteMessage(PingMessage, make([]byte, 126)); err == nil {
		t.Fatal("long ping accepted")
	}
}

func TestServerFramesUnmaskedClientFramesMasked(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	client := newConn(a, nil, false)
	go client.WriteMessage(BinaryMessage, []byte("abc"))
	var h [2]byte
	io.ReadFull(b, h[:])
	if h[1]&0x80 == 0 {
		t.Fatal("client frame not masked")
	}
	rest := make([]byte, 4+3)
	io.ReadFull(b, rest)
	var key [4]byte
	copy(key[:], rest)
	p := rest[4:]
	maskBytes(key, 0, p)
	if string(p) != "abc" {
		t.Fatalf("unmasked %q", p)
	}
}
