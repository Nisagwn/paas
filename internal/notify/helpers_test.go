package notify_test

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nisagwn/paas/internal/notify"
	"github.com/nisagwn/paas/internal/store"
	"github.com/nisagwn/paas/internal/testdb"
)

var discard = slog.New(slog.NewTextHandler(io.Discard, nil))

// clock is a settable time source.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) Add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// fixture: team "web" (owner alice) with app "blog" and "shop".
type fixture struct {
	t     *testing.T
	st    *store.Store
	team  store.Team
	blog  store.App
	shop  store.App
	clock *clock
	svc   *notify.Service
	disp  *notify.Dispatcher
}

func setup(t *testing.T) *fixture {
	t.Helper()
	st := testdb.Open(t)
	ctx := context.Background()
	alice, err := st.UpsertUser(ctx, 1, "alice", "", "")
	if err != nil {
		t.Fatal(err)
	}
	team, err := st.CreateTeam(ctx, "web", "Web", alice.ID)
	if err != nil {
		t.Fatal(err)
	}
	blog, _ := st.CreateAppInTeam(ctx, team.ID, "blog", "nisagwn/blog", "main")
	shop, _ := st.CreateAppInTeam(ctx, team.ID, "shop", "nisagwn/shop", "main")
	// Rows are queued at the database's now(); the dispatcher's clock
	// starts a little later so a skew between the clocks does not matter.
	c := &clock{t: time.Now().UTC().Add(time.Minute)}
	client := notify.NewHTTPClient(5*time.Second, true)
	d := &notify.Dispatcher{
		Store: st, Log: discard, Now: c.Now, Jitter: func() float64 { return 1 },
		BaseBackoff: 30 * time.Second, MaxAttempts: 4,
		Senders: map[string]notify.Sender{
			store.ChannelSlack:   notify.SlackSender{Client: client},
			store.ChannelDiscord: notify.DiscordSender{Client: client},
			store.ChannelWebhook: notify.WebhookSender{Client: client, Now: c.Now},
		},
	}
	svc := &notify.Service{Store: st, PublicURL: "https://paas.example.com", Dispatcher: d, Log: discard, Now: c.Now,
		Validation: notify.Validation{AllowInsecureURLs: true}}
	return &fixture{t: t, st: st, team: team, blog: blog, shop: shop, clock: c, svc: svc, disp: d}
}

func (f *fixture) channel(req notify.ChannelRequest) (store.NotificationChannel, string) {
	f.t.Helper()
	ch, secret, err := f.svc.CreateChannel(context.Background(), f.team.ID, req, "alice")
	if err != nil {
		f.t.Fatalf("create channel: %v", err)
	}
	return ch, secret
}

func (f *fixture) deliveries(ch store.NotificationChannel) []store.NotificationDelivery {
	f.t.Helper()
	ds, err := f.st.ListNotificationDeliveries(context.Background(), ch.ID, 100)
	if err != nil {
		f.t.Fatal(err)
	}
	return ds
}

func strs(s ...string) *[]string { return &s }

// receiver is an httptest server answering with the queued statuses
// (200 when the queue is empty) and recording requests.
type receiver struct {
	*httptest.Server
	mu       sync.Mutex
	statuses []int
	headers  []http.Header
	bodies   [][]byte
	retry    string
}

func newReceiver(t *testing.T, statuses ...int) *receiver {
	r := &receiver{statuses: statuses}
	r.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		b, _ := io.ReadAll(req.Body)
		r.mu.Lock()
		r.headers = append(r.headers, req.Header.Clone())
		r.bodies = append(r.bodies, b)
		code := http.StatusOK
		if len(r.statuses) > 0 {
			code, r.statuses = r.statuses[0], r.statuses[1:]
		}
		retry := r.retry
		r.mu.Unlock()
		if retry != "" && code == http.StatusTooManyRequests {
			w.Header().Set("Retry-After", retry)
		}
		w.WriteHeader(code)
		io.WriteString(w, "ok")
	}))
	t.Cleanup(r.Close)
	return r
}

func (r *receiver) count() int { r.mu.Lock(); defer r.mu.Unlock(); return len(r.bodies) }

// ---- fake SMTP server ----

type smtpMsg struct {
	From, To string
	Data     string
}

type fakeSMTP struct {
	addr     string
	tlsCfg   *tls.Config
	starttls bool
	reject   map[string]bool
	mu       sync.Mutex
	msgs     []smtpMsg
	auth     []string
	tlsUsed  bool
}

func selfSigned(t *testing.T) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "localhost"},
		DNSNames: []string{"localhost"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, pool
}

// newFakeSMTP speaks enough SMTP for net/smtp: EHLO, STARTTLS, AUTH
// PLAIN, MAIL, RCPT (550 for rejected addresses), DATA, RSET, QUIT.
func newFakeSMTP(t *testing.T, starttls bool) (*fakeSMTP, *x509.CertPool) {
	cert, pool := selfSigned(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	s := &fakeSMTP{addr: ln.Addr().String(), starttls: starttls, reject: map[string]bool{},
		tlsCfg: &tls.Config{Certificates: []tls.Certificate{cert}}}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go s.serve(c)
		}
	}()
	return s, pool
}

func (s *fakeSMTP) serve(c net.Conn) {
	defer c.Close()
	r := bufio.NewReader(c)
	w := func(line string) { io.WriteString(c, line+"\r\n") }
	w("220 fake ESMTP")
	secure := false
	var from string
	var to []string
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		cmd := strings.ToUpper(strings.SplitN(line, " ", 2)[0])
		switch cmd {
		case "EHLO", "HELO":
			if s.starttls && !secure {
				w("250-fake")
				w("250 STARTTLS")
			} else {
				w("250-fake")
				w("250 AUTH PLAIN")
			}
		case "STARTTLS":
			w("220 go ahead")
			tc := tls.Server(c, s.tlsCfg)
			if err := tc.Handshake(); err != nil {
				return
			}
			c, r, secure = tc, bufio.NewReader(tc), true
			s.mu.Lock()
			s.tlsUsed = true
			s.mu.Unlock()
		case "AUTH":
			parts := strings.Fields(line)
			if len(parts) == 3 {
				b, _ := base64.StdEncoding.DecodeString(parts[2])
				s.mu.Lock()
				s.auth = append(s.auth, string(b))
				s.mu.Unlock()
			}
			w("235 ok")
		case "MAIL":
			from, to = strings.TrimSuffix(strings.TrimPrefix(line[10:], "<"), ">"), nil
			w("250 ok")
		case "RCPT":
			addr := strings.TrimSuffix(strings.TrimPrefix(line[8:], "<"), ">")
			if s.reject[addr] {
				w("550 no such user")
				continue
			}
			to = append(to, addr)
			w("250 ok")
		case "DATA":
			w("354 go")
			var b strings.Builder
			for {
				l, err := r.ReadString('\n')
				if err != nil {
					return
				}
				if l == ".\r\n" {
					break
				}
				l = strings.TrimPrefix(l, ".") // dot-stuffing (RFC 5321 4.5.2)
				b.WriteString(l)
			}
			s.mu.Lock()
			for _, t := range to {
				s.msgs = append(s.msgs, smtpMsg{From: from, To: t, Data: b.String()})
			}
			s.mu.Unlock()
			w("250 queued")
		case "RSET":
			w("250 ok")
		case "QUIT":
			w("221 bye")
			return
		default:
			w("502 unknown")
		}
	}
}

func (s *fakeSMTP) messages() []smtpMsg {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]smtpMsg{}, s.msgs...)
}
