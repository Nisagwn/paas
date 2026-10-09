// Package wsock is a WebSocket (RFC 6455) implementation on the standard
// library: the opening handshake (server Upgrade and client Dial), text and
// binary messages (fragmented on read), ping / pong, the closing handshake
// with status codes, and client-to-server masking.
//
// It covers what the browser terminal (Faz 27) needs and nothing more: no
// extensions (permessage-deflate is never negotiated), no subprotocol
// negotiation beyond echoing one the caller picks, and every message is
// written as a single frame.
//
// A Conn supports one concurrent reader and any number of concurrent
// writers. ReadMessage answers pings and the peer's close frame itself.
package wsock

import (
	"bufio"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Message types (frame opcodes).
const (
	continuationFrame = 0
	TextMessage       = 1
	BinaryMessage     = 2
	CloseMessage      = 8
	PingMessage       = 9
	PongMessage       = 10
)

// Close status codes (RFC 6455 §7.4.1). 4000–4999 are free for
// applications.
const (
	CloseNormal          = 1000
	CloseGoingAway       = 1001
	CloseProtocolError   = 1002
	CloseUnsupportedData = 1003
	CloseNoStatus        = 1005 // never sent: the peer's close frame had no code
	CloseAbnormal        = 1006 // never sent: the connection dropped without a close frame
	CloseInvalidPayload  = 1007
	ClosePolicyViolation = 1008
	CloseTooBig          = 1009
	CloseInternalError   = 1011
)

// guid is the fixed key suffix of the handshake (RFC 6455 §1.3).
const guid = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

const (
	// DefaultMaxMessage bounds a (reassembled) incoming message.
	DefaultMaxMessage = 1 << 20
	// DefaultWriteTimeout bounds every write.
	DefaultWriteTimeout = 10 * time.Second
	maxControlPayload   = 125
)

// CloseError is returned by ReadMessage once the connection is closing: the
// peer sent a close frame (Code is its status, CloseNoStatus without one),
// or this side found a protocol violation and closed with Code.
type CloseError struct {
	Code int
	Text string
	// Local: this side closed because the peer broke the protocol.
	Local bool
}

func (e *CloseError) Error() string {
	who := "peer"
	if e.Local {
		who = "local"
	}
	if e.Text != "" {
		return fmt.Sprintf("websocket: closed by %s: %d %s", who, e.Code, e.Text)
	}
	return fmt.Sprintf("websocket: closed by %s: %d", who, e.Code)
}

// IsClose reports whether err is a CloseError with one of codes (any code
// when none are given).
func IsClose(err error, codes ...int) bool {
	var ce *CloseError
	if !errors.As(err, &ce) {
		return false
	}
	if len(codes) == 0 {
		return true
	}
	for _, c := range codes {
		if ce.Code == c {
			return true
		}
	}
	return false
}

// ErrClosed is returned when writing after the close frame was sent.
var ErrClosed = errors.New("websocket: connection closed")

// HandshakeError is a refused opening handshake.
type HandshakeError struct {
	Status int
	Reason string
}

func (e *HandshakeError) Error() string { return "websocket: bad handshake: " + e.Reason }

// IsUpgrade reports whether r asks for a WebSocket upgrade.
func IsUpgrade(r *http.Request) bool {
	return headerHasToken(r.Header, "Connection", "upgrade") && headerHasToken(r.Header, "Upgrade", "websocket")
}

// AcceptKey computes Sec-WebSocket-Accept for a Sec-WebSocket-Key.
func AcceptKey(key string) string {
	h := sha1.New()
	h.Write([]byte(key + guid))
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

// Upgrade completes the server side of the opening handshake and takes over
// the connection. Origin checks are the caller's job, before calling it. On
// a malformed request it answers with an HTTP error and returns a
// *HandshakeError. header is added to the 101 response.
func Upgrade(w http.ResponseWriter, r *http.Request, header http.Header) (*Conn, error) {
	fail := func(status int, reason string) (*Conn, error) {
		if status == http.StatusUpgradeRequired {
			w.Header().Set("Sec-WebSocket-Version", "13")
		}
		http.Error(w, "websocket: "+reason, status)
		return nil, &HandshakeError{Status: status, Reason: reason}
	}
	switch {
	case r.Method != http.MethodGet:
		return fail(http.StatusMethodNotAllowed, "method must be GET")
	case !IsUpgrade(r):
		return fail(http.StatusBadRequest, "not a websocket upgrade request")
	case r.Header.Get("Sec-WebSocket-Version") != "13":
		return fail(http.StatusUpgradeRequired, "unsupported version (want 13)")
	}
	key := strings.TrimSpace(r.Header.Get("Sec-WebSocket-Key"))
	if raw, err := base64.StdEncoding.DecodeString(key); err != nil || len(raw) != 16 {
		return fail(http.StatusBadRequest, "invalid Sec-WebSocket-Key")
	}
	netConn, brw, err := http.NewResponseController(w).Hijack()
	if err != nil {
		return fail(http.StatusInternalServerError, "connection cannot be taken over: "+err.Error())
	}
	var b strings.Builder
	b.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: ")
	b.WriteString(AcceptKey(key))
	b.WriteString("\r\n")
	for k, vs := range header {
		for _, v := range vs {
			b.WriteString(k + ": " + strings.NewReplacer("\r", "", "\n", "").Replace(v) + "\r\n")
		}
	}
	b.WriteString("\r\n")
	netConn.SetWriteDeadline(time.Now().Add(DefaultWriteTimeout))
	if _, err := netConn.Write([]byte(b.String())); err != nil {
		netConn.Close()
		return nil, err
	}
	netConn.SetDeadline(time.Time{})
	// Bytes the client sent right after its request (its first frames)
	// may already sit in the server's buffer.
	return newConn(netConn, brw.Reader, true), nil
}

// Conn is one WebSocket connection.
type Conn struct {
	conn   net.Conn
	br     *bufio.Reader
	server bool

	// MaxMessage bounds an incoming message; larger ones close the
	// connection with CloseTooBig. Zero means DefaultMaxMessage.
	MaxMessage int64
	// WriteTimeout bounds each write; zero means DefaultWriteTimeout.
	WriteTimeout time.Duration

	wmu       sync.Mutex
	closeSent bool

	// readErr is sticky: after a close or a protocol error every
	// ReadMessage returns it.
	readErr error
}

func newConn(c net.Conn, br *bufio.Reader, server bool) *Conn {
	if br == nil {
		br = bufio.NewReader(c)
	}
	return &Conn{conn: c, br: br, server: server}
}

// NetConn returns the underlying connection.
func (c *Conn) NetConn() net.Conn { return c.conn }

// RemoteAddr is the peer's address.
func (c *Conn) RemoteAddr() net.Addr { return c.conn.RemoteAddr() }

// SetReadDeadline bounds the next reads; a passed deadline makes
// ReadMessage fail with a timeout (not a CloseError).
func (c *Conn) SetReadDeadline(t time.Time) error { return c.conn.SetReadDeadline(t) }

// Close closes the network connection without a closing handshake; use
// WriteClose first for a clean close.
func (c *Conn) Close() error { return c.conn.Close() }

// ---- writing ----

// WriteMessage sends one message as a single frame. Control messages
// (Ping, Pong, Close) carry at most 125 bytes. Safe for concurrent use.
func (c *Conn) WriteMessage(op int, data []byte) error {
	switch op {
	case TextMessage, BinaryMessage:
	case PingMessage, PongMessage, CloseMessage:
		if len(data) > maxControlPayload {
			return errors.New("websocket: control frame payload over 125 bytes")
		}
	default:
		return fmt.Errorf("websocket: unknown message type %d", op)
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if c.closeSent {
		return ErrClosed
	}
	if op == CloseMessage {
		c.closeSent = true
	}
	return c.writeFrame(true, op, data)
}

// WriteClose starts (or answers) the closing handshake with code and a
// reason (trimmed to fit the frame). After it, reads should continue until
// ReadMessage returns the peer's close (or fails), then Close.
func (c *Conn) WriteClose(code int, reason string) error {
	return c.WriteMessage(CloseMessage, closePayload(code, reason))
}

func closePayload(code int, reason string) []byte {
	if code == CloseNoStatus {
		return nil
	}
	if len(reason) > maxControlPayload-2 {
		reason = reason[:maxControlPayload-2]
		for !utf8.ValidString(reason) { // do not cut a rune in half
			reason = reason[:len(reason)-1]
		}
	}
	p := make([]byte, 2+len(reason))
	binary.BigEndian.PutUint16(p, uint16(code))
	copy(p[2:], reason)
	return p
}

// writeFrame writes one frame; the caller holds wmu.
func (c *Conn) writeFrame(fin bool, op int, data []byte) error {
	var hdr [14]byte
	hdr[0] = byte(op)
	if fin {
		hdr[0] |= 0x80
	}
	n := 2
	switch l := len(data); {
	case l <= 125:
		hdr[1] = byte(l)
	case l <= 0xffff:
		hdr[1] = 126
		binary.BigEndian.PutUint16(hdr[2:], uint16(l))
		n += 2
	default:
		hdr[1] = 127
		binary.BigEndian.PutUint64(hdr[2:], uint64(l))
		n += 8
	}
	payload := data
	if !c.server { // clients mask every frame (RFC 6455 §5.3)
		hdr[1] |= 0x80
		var key [4]byte
		rand.Read(key[:])
		copy(hdr[n:], key[:])
		n += 4
		payload = make([]byte, len(data))
		copy(payload, data)
		maskBytes(key, 0, payload)
	}
	timeout := c.WriteTimeout
	if timeout <= 0 {
		timeout = DefaultWriteTimeout
	}
	c.conn.SetWriteDeadline(time.Now().Add(timeout))
	buf := make([]byte, 0, n+len(payload))
	buf = append(append(buf, hdr[:n]...), payload...)
	_, err := c.conn.Write(buf)
	return err
}

func maskBytes(key [4]byte, pos int, b []byte) int {
	for i := range b {
		b[i] ^= key[pos&3]
		pos++
	}
	return pos & 3
}

// ---- reading ----

type frame struct {
	fin     bool
	op      int
	payload []byte
}

// ReadMessage returns the next text or binary message, reassembling
// fragments. It answers pings with pongs and skips pongs. When the peer
// closes, it answers the close frame and returns a *CloseError; on a
// protocol violation it closes with the matching code and also returns a
// *CloseError (Local set). Network errors are returned as they are.
func (c *Conn) ReadMessage() (int, []byte, error) {
	if c.readErr != nil {
		return 0, nil, c.readErr
	}
	op, data, err := c.readMessage()
	if err != nil {
		var ce *CloseError
		if errors.As(err, &ce) {
			c.readErr = err
		}
		return 0, nil, err
	}
	return op, data, nil
}

func (c *Conn) readMessage() (int, []byte, error) {
	limit := c.MaxMessage
	if limit <= 0 {
		limit = DefaultMaxMessage
	}
	var (
		msgOp int
		msg   []byte
	)
	for {
		f, err := c.readFrame(limit)
		if err != nil {
			return 0, nil, err
		}
		switch f.op {
		case PingMessage:
			if err := c.WriteMessage(PongMessage, f.payload); err != nil && !errors.Is(err, ErrClosed) {
				return 0, nil, err
			}
			continue
		case PongMessage:
			continue
		case CloseMessage:
			return 0, nil, c.peerClosed(f.payload)
		case continuationFrame:
			if msgOp == 0 {
				return 0, nil, c.fail(CloseProtocolError, "continuation frame without a message")
			}
		case TextMessage, BinaryMessage:
			if msgOp != 0 {
				return 0, nil, c.fail(CloseProtocolError, "new message inside a fragmented one")
			}
			msgOp = f.op
		}
		if int64(len(msg))+int64(len(f.payload)) > limit {
			return 0, nil, c.fail(CloseTooBig, "message too big")
		}
		msg = append(msg, f.payload...)
		if !f.fin {
			continue
		}
		if msgOp == TextMessage && !utf8.Valid(msg) {
			return 0, nil, c.fail(CloseInvalidPayload, "text message is not valid UTF-8")
		}
		if msg == nil {
			msg = []byte{}
		}
		return msgOp, msg, nil
	}
}

// readFrame reads and validates one frame.
func (c *Conn) readFrame(limit int64) (frame, error) {
	var h [2]byte
	if _, err := io.ReadFull(c.br, h[:]); err != nil {
		return frame{}, err
	}
	f := frame{fin: h[0]&0x80 != 0, op: int(h[0] & 0x0f)}
	if h[0]&0x70 != 0 {
		return f, c.fail(CloseProtocolError, "reserved bits set without an extension")
	}
	masked := h[1]&0x80 != 0
	length := int64(h[1] & 0x7f)
	switch length {
	case 126:
		var b [2]byte
		if _, err := io.ReadFull(c.br, b[:]); err != nil {
			return f, err
		}
		length = int64(binary.BigEndian.Uint16(b[:]))
	case 127:
		var b [8]byte
		if _, err := io.ReadFull(c.br, b[:]); err != nil {
			return f, err
		}
		u := binary.BigEndian.Uint64(b[:])
		if u>>63 != 0 {
			return f, c.fail(CloseProtocolError, "invalid frame length")
		}
		length = int64(u)
	}
	switch f.op {
	case continuationFrame, TextMessage, BinaryMessage:
	case CloseMessage, PingMessage, PongMessage:
		if !f.fin {
			return f, c.fail(CloseProtocolError, "fragmented control frame")
		}
		if length > maxControlPayload {
			return f, c.fail(CloseProtocolError, "control frame payload over 125 bytes")
		}
	default:
		return f, c.fail(CloseProtocolError, fmt.Sprintf("unknown opcode %d", f.op))
	}
	if c.server && !masked {
		return f, c.fail(CloseProtocolError, "client frames must be masked")
	}
	if !c.server && masked {
		return f, c.fail(CloseProtocolError, "server frames must not be masked")
	}
	if length > limit {
		return f, c.fail(CloseTooBig, "message too big")
	}
	var key [4]byte
	if masked {
		if _, err := io.ReadFull(c.br, key[:]); err != nil {
			return f, err
		}
	}
	f.payload = make([]byte, length)
	if _, err := io.ReadFull(c.br, f.payload); err != nil {
		return f, err
	}
	if masked {
		maskBytes(key, 0, f.payload)
	}
	return f, nil
}

// peerClosed validates the peer's close frame, answers it (echoing the
// code) and returns the CloseError.
func (c *Conn) peerClosed(p []byte) error {
	ce := &CloseError{Code: CloseNoStatus}
	switch {
	case len(p) == 1:
		return c.fail(CloseProtocolError, "close frame with a 1-byte payload")
	case len(p) >= 2:
		ce.Code = int(binary.BigEndian.Uint16(p))
		if !validCloseCode(ce.Code) {
			return c.fail(CloseProtocolError, fmt.Sprintf("invalid close code %d", ce.Code))
		}
		if !utf8.Valid(p[2:]) {
			return c.fail(CloseInvalidPayload, "close reason is not valid UTF-8")
		}
		ce.Text = string(p[2:])
	}
	if err := c.WriteClose(ce.Code, ""); err != nil && !errors.Is(err, ErrClosed) {
		return err
	}
	return ce
}

// fail closes the connection for a protocol violation.
func (c *Conn) fail(code int, reason string) error {
	c.WriteClose(code, reason)
	return &CloseError{Code: code, Text: reason, Local: true}
}

// validCloseCode reports whether a code may appear in a close frame.
func validCloseCode(code int) bool {
	switch {
	case code >= 1000 && code <= 1003, code >= 1007 && code <= 1014:
		return true
	case code >= 3000 && code <= 4999:
		return true
	}
	return false
}

func headerHasToken(h http.Header, name, token string) bool {
	for _, v := range h.Values(name) {
		for _, t := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(t), token) {
				return true
			}
		}
	}
	return false
}

var timeZero time.Time
