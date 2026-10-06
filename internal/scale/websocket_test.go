package scale

import (
	"bufio"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Faz 20: WebSocket servers are a first-class app type, so the first
// connection to a sleeping deployment must survive the wake: the activator
// tunnels the Upgrade and keeps the connection open past its timeout.

const wsGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

func wsAccept(key string) string {
	sum := sha1.Sum([]byte(key + wsGUID))
	return base64.StdEncoding.EncodeToString(sum[:])
}

// wsWrite writes one text frame (payload < 126 bytes); clients mask.
func wsWrite(w io.Writer, msg string, mask bool) error {
	b := []byte{0x81, byte(len(msg))}
	payload := []byte(msg)
	if mask {
		key := []byte{1, 2, 3, 4}
		b[1] |= 0x80
		b = append(b, key...)
		for i := range payload {
			payload[i] ^= key[i%4]
		}
	}
	_, err := w.Write(append(b, payload...))
	return err
}

// wsRead reads one short frame and returns its text.
func wsRead(r *bufio.Reader) (string, error) {
	var h [2]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return "", err
	}
	n := int(h[1] & 0x7f)
	var key [4]byte
	masked := h[1]&0x80 != 0
	if masked {
		if _, err := io.ReadFull(r, key[:]); err != nil {
			return "", err
		}
	}
	p := make([]byte, n)
	if _, err := io.ReadFull(r, p); err != nil {
		return "", err
	}
	if masked {
		for i := range p {
			p[i] ^= key[i%4]
		}
	}
	return string(p), nil
}

// echoServer is a WebSocket echo server written against RFC 6455.
func echoServer(t *testing.T) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
			http.Error(w, "not a websocket request", http.StatusBadRequest)
			return
		}
		conn, rw, err := http.NewResponseController(w).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		fmt.Fprintf(rw, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n"+
			"Sec-WebSocket-Accept: %s\r\n\r\n", wsAccept(r.Header.Get("Sec-WebSocket-Key")))
		rw.Flush()
		for {
			msg, err := wsRead(rw.Reader)
			if err != nil {
				return
			}
			if err := wsWrite(conn, "echo: "+msg, false); err != nil {
				return
			}
		}
	}))
}

func TestActivatorProxiesWebSocket(t *testing.T) {
	e := newEnv(t, false)
	e.cluster.Sleep(context.Background(), "app-blog", "d-1111111")
	app := echoServer(t)
	defer app.Close()
	e.cluster.podAddr = strings.TrimPrefix(app.URL, "http://")

	timeout := 300 * time.Millisecond
	a := &Activator{Scaler: e.s, Cluster: e.cluster, Upstream: "pod", Timeout: timeout, Log: quiet}
	if err := a.Init(); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(a)
	defer srv.Close()

	conn, err := net.Dial("tcp", strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(10 * time.Second))
	const key = "dGhlIHNhbXBsZSBub25jZQ=="
	fmt.Fprintf(conn, "GET /chat HTTP/1.1\r\nHost: 1111111-blog.paas.test\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n"+
		"Sec-WebSocket-Key: %s\r\nSec-WebSocket-Version: 13\r\n\r\n", key)
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols || resp.Header.Get("Sec-WebSocket-Accept") != wsAccept(key) ||
		!strings.EqualFold(resp.Header.Get("Upgrade"), "websocket") {
		t.Fatalf("handshake: %d %v", resp.StatusCode, resp.Header)
	}
	if e.cluster.get(prevKey).Sleeping {
		t.Fatal("deployment not woken")
	}

	for i, msg := range []string{"hello", "still here"} {
		if i == 1 {
			// Past the activator's timeout: the tunnel must stay open.
			time.Sleep(2 * timeout)
		}
		if err := wsWrite(conn, msg, true); err != nil {
			t.Fatal(err)
		}
		got, err := wsRead(br)
		if err != nil {
			t.Fatalf("message %d: %v", i, err)
		}
		if got != "echo: "+msg {
			t.Fatalf("message %d = %q", i, got)
		}
	}
}
