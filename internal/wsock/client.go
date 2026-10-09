package wsock

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
)

// Dial opens a client connection to a ws:// or wss:// URL, sending header
// with the handshake (e.g. Origin, Cookie). The client masks its frames.
// It is used by tests and command-line checks; browsers are the real
// clients of the terminal.
func Dial(ctx context.Context, rawURL string, header http.Header) (*Conn, *http.Response, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, nil, err
	}
	host := u.Host
	switch u.Scheme {
	case "ws":
		if u.Port() == "" {
			host = net.JoinHostPort(u.Hostname(), "80")
		}
	case "wss":
		if u.Port() == "" {
			host = net.JoinHostPort(u.Hostname(), "443")
		}
	default:
		return nil, nil, fmt.Errorf("websocket: unsupported scheme %q", u.Scheme)
	}
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", host)
	if err != nil {
		return nil, nil, err
	}
	if u.Scheme == "wss" {
		tc := tls.Client(conn, &tls.Config{ServerName: u.Hostname()})
		if err := tc.HandshakeContext(ctx); err != nil {
			conn.Close()
			return nil, nil, err
		}
		conn = tc
	}
	if dl, ok := ctx.Deadline(); ok {
		conn.SetDeadline(dl)
	}

	raw := make([]byte, 16)
	rand.Read(raw)
	key := base64.StdEncoding.EncodeToString(raw)
	req := &http.Request{
		Method: http.MethodGet, URL: u, Host: u.Host, Header: http.Header{},
		Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1,
	}
	for k, vs := range header {
		req.Header[k] = vs
	}
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Sec-WebSocket-Key", key)
	req.Header.Set("Sec-WebSocket-Version", "13")
	// http.Request.Write uses the URL's scheme only for the request line.
	u2 := *u
	u2.Scheme = strings.Replace(u2.Scheme, "ws", "http", 1)
	req.URL = &u2
	if err := req.Write(conn); err != nil {
		conn.Close()
		return nil, nil, err
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		conn.Close()
		return nil, nil, err
	}
	if resp.StatusCode != http.StatusSwitchingProtocols ||
		!headerHasToken(resp.Header, "Upgrade", "websocket") ||
		resp.Header.Get("Sec-WebSocket-Accept") != AcceptKey(key) {
		conn.Close()
		return nil, resp, &HandshakeError{Status: resp.StatusCode, Reason: "server answered " + resp.Status}
	}
	conn.SetDeadline(timeZero)
	return newConn(conn, br, false), resp, nil
}
