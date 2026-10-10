package notify

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/nisagwn/paas/internal/store"
)

// Message is one delivery as a Sender sees it.
type Message struct {
	Event   Event
	Channel store.NotificationChannel
	Secrets store.ChannelSecrets
	// Recipients of an email channel (preferences applied).
	Recipients []string
	// IdempotencyKey is stable across retries of this event on this channel.
	IdempotencyKey string
	Attempt        int
}

// Result is a successful delivery. Note is stored in the delivery log
// (e.g. addresses an SMTP server rejected while others were accepted).
type Result struct {
	Code int
	Note string
}

// Sender delivers to one channel kind.
type Sender interface {
	Send(ctx context.Context, m Message) (Result, error)
}

// SendError is a failed attempt. Permanent errors are not retried;
// RetryAfter (from a 429) delays the next attempt.
type SendError struct {
	Code       int
	Msg        string
	Permanent  bool
	RetryAfter time.Duration
}

func (e *SendError) Error() string {
	if e.Code != 0 {
		return fmt.Sprintf("HTTP %d: %s", e.Code, e.Msg)
	}
	return e.Msg
}

func permanent(msg string) error { return &SendError{Msg: msg, Permanent: true} }

// ---- HTTP ----

// errPrivateAddress: a webhook URL that resolves to the control plane's
// own network (SSRF).
var errPrivateAddress = errors.New("adres özel ya da yerel bir ağda; güvenlik nedeniyle izin verilmiyor")

// PublicIP reports whether ip is a globally routable unicast address.
func PublicIP(ip net.IP) bool {
	if ip == nil || ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() || ip.IsMulticast() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsInterfaceLocalMulticast() {
		return false
	}
	if v4 := ip.To4(); v4 != nil {
		// Carrier-grade NAT 100.64.0.0/10, 0.0.0.0/8, 192.0.0.0/24, benchmarking 198.18.0.0/15.
		switch {
		case v4[0] == 100 && v4[1]&0xC0 == 64, v4[0] == 0, v4[0] == 192 && v4[1] == 0 && v4[2] == 0,
			v4[0] == 198 && (v4[1] == 18 || v4[1] == 19), v4[0] >= 240:
			return false
		}
	}
	return true
}

// NewHTTPClient returns the client of the webhook senders: no proxy, no
// redirects, a timeout, and — unless allowPrivate — connections only to
// public addresses, checked after DNS resolution so a hostname cannot
// point into the cluster or the cloud metadata service.
func NewHTTPClient(timeout time.Duration, allowPrivate bool) *http.Client {
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	dialer := &net.Dialer{Timeout: timeout}
	if !allowPrivate {
		dialer.Control = func(network, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}
			if !PublicIP(net.ParseIP(host)) {
				return errPrivateAddress
			}
			return nil
		}
	}
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			Proxy: nil, DialContext: dialer.DialContext, ForceAttemptHTTP2: true,
			TLSHandshakeTimeout: timeout, ResponseHeaderTimeout: timeout, MaxIdleConnsPerHost: 2,
			IdleConnTimeout: 90 * time.Second,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// post sends body and classifies the response.
func post(ctx context.Context, client *http.Client, rawURL string, body []byte, header http.Header) (Result, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rawURL, bytes.NewReader(body))
	if err != nil {
		return Result{}, permanent("geçersiz URL")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "paas-notifications/1")
	for k, v := range header {
		req.Header[k] = v
	}
	resp, err := client.Do(req)
	if err != nil {
		return Result{}, transportError(err)
	}
	defer resp.Body.Close()
	snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	msg := oneLine(string(snippet))
	if msg == "" {
		msg = http.StatusText(resp.StatusCode)
	}
	msg = clip(msg, 200)
	switch c := resp.StatusCode; {
	case c >= 200 && c < 300:
		return Result{Code: c}, nil
	case c == http.StatusTooManyRequests:
		return Result{}, &SendError{Code: c, Msg: "hız sınırı: " + msg, RetryAfter: retryAfter(resp.Header.Get("Retry-After"))}
	case c >= 500 || c == http.StatusRequestTimeout:
		return Result{}, &SendError{Code: c, Msg: msg}
	case c >= 300 && c < 400:
		return Result{}, &SendError{Code: c, Msg: "yönlendirme izlenmez; URL'yi kontrol et", Permanent: true}
	default:
		return Result{}, &SendError{Code: c, Msg: msg, Permanent: true}
	}
}

// transportError drops the URL from net/http errors: webhook URLs are
// secrets and the delivery log is readable by viewers.
func transportError(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	if errors.Is(err, errPrivateAddress) {
		return permanent(errPrivateAddress.Error())
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return &SendError{Msg: "zaman aşımı"}
	}
	var dns *net.DNSError
	if errors.As(err, &dns) {
		return &SendError{Msg: "DNS çözümlenemedi: " + dns.Err}
	}
	var op *net.OpError
	if errors.As(err, &op) && op.Err != nil {
		return &SendError{Msg: "bağlantı kurulamadı: " + clip(op.Err.Error(), 160)}
	}
	return &SendError{Msg: "bağlantı hatası: " + clip(err.Error(), 160)}
}

// retryAfter parses seconds or an HTTP date; 0 when absent.
func retryAfter(v string) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if n, err := strconv.ParseFloat(v, 64); err == nil && n >= 0 {
		return time.Duration(n * float64(time.Second))
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

// SlackSender posts to Slack incoming webhooks.
type SlackSender struct{ Client *http.Client }

func (s SlackSender) Send(ctx context.Context, m Message) (Result, error) {
	body, err := SlackPayload(m.Event)
	if err != nil {
		return Result{}, permanent(err.Error())
	}
	return post(ctx, s.Client, m.Secrets.URL, body, nil)
}

// DiscordSender posts to Discord webhooks.
type DiscordSender struct{ Client *http.Client }

func (s DiscordSender) Send(ctx context.Context, m Message) (Result, error) {
	body, err := DiscordPayload(m.Event)
	if err != nil {
		return Result{}, permanent(err.Error())
	}
	return post(ctx, s.Client, m.Secrets.URL, body, nil)
}

// Webhook headers.
const (
	HeaderSignature = "X-Paas-Signature"
	HeaderTimestamp = "X-Paas-Timestamp"
	HeaderEvent     = "X-Paas-Event"
	HeaderDelivery  = "X-Paas-Delivery"
)

// Sign returns the X-Paas-Signature value: "sha256=" and the hex
// HMAC-SHA256 of "<timestamp>.<body>" under the channel's signing secret.
// The timestamp lets receivers reject old (replayed) deliveries.
func Sign(secret string, timestamp int64, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(strconv.FormatInt(timestamp, 10)))
	mac.Write([]byte{'.'})
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// VerifySignature checks a delivery the way a receiver should: constant
// time comparison and a timestamp no older than tolerance.
func VerifySignature(secret string, header http.Header, body []byte, now time.Time, tolerance time.Duration) error {
	ts, err := strconv.ParseInt(header.Get(HeaderTimestamp), 10, 64)
	if err != nil {
		return errors.New("missing or invalid " + HeaderTimestamp)
	}
	if d := now.Sub(time.Unix(ts, 0)); d > tolerance || d < -tolerance {
		return errors.New("timestamp outside the tolerance")
	}
	if !hmac.Equal([]byte(Sign(secret, ts, body)), []byte(header.Get(HeaderSignature))) {
		return errors.New("signature mismatch")
	}
	return nil
}

// WebhookSender posts signed JSON to any URL.
type WebhookSender struct {
	Client *http.Client
	// Now is the clock of the timestamp (tests); nil is time.Now.
	Now func() time.Time
}

func (s WebhookSender) Send(ctx context.Context, m Message) (Result, error) {
	body, err := WebhookPayload(m.Event, m.IdempotencyKey)
	if err != nil {
		return Result{}, permanent(err.Error())
	}
	now := time.Now
	if s.Now != nil {
		now = s.Now
	}
	ts := now().Unix()
	h := http.Header{}
	h.Set(HeaderEvent, m.Event.Type)
	h.Set(HeaderDelivery, m.IdempotencyKey)
	h.Set(HeaderTimestamp, strconv.FormatInt(ts, 10))
	h.Set("Idempotency-Key", m.IdempotencyKey)
	if m.Secrets.SigningSecret != "" {
		h.Set(HeaderSignature, Sign(m.Secrets.SigningSecret, ts, body))
	}
	return post(ctx, s.Client, m.Secrets.URL, body, h)
}
