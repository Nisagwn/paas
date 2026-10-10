package notify

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"mime"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"net/smtp"
	"net/textproto"
	"strconv"
	"strings"
	"time"
)

// SMTP security modes.
const (
	SMTPStartTLS = "starttls" // default: plain connection upgraded with STARTTLS (587)
	SMTPTLS      = "tls"      // implicit TLS (465)
	SMTPNone     = "none"     // no TLS: development relays (MailHog) only, no AUTH
)

// SMTPConfig is the outgoing mail server (PAAS_SMTP_*).
type SMTPConfig struct {
	Host     string
	Port     int
	User     string
	Password string
	From     string
	// Security is SMTPStartTLS (default), SMTPTLS or SMTPNone.
	Security string
	// TLSConfig overrides the TLS settings (tests trust their own CA).
	TLSConfig *tls.Config
	Timeout   time.Duration
	// LocalName is sent in EHLO; default "paas".
	LocalName string
}

// Enabled reports whether an SMTP server is configured.
func (c SMTPConfig) Enabled() bool { return c.Host != "" }

// Check validates the configuration.
func (c SMTPConfig) Check() error {
	if !c.Enabled() {
		return nil
	}
	if _, err := mail.ParseAddress(c.From); err != nil {
		return fmt.Errorf("PAAS_SMTP_FROM must be an email address (e.g. paas@example.com): %v", err)
	}
	switch c.Security {
	case "", SMTPStartTLS, SMTPTLS, SMTPNone:
	default:
		return fmt.Errorf("PAAS_SMTP_SECURITY must be starttls, tls or none")
	}
	if c.Security == SMTPNone && c.User != "" {
		return errors.New("PAAS_SMTP_SECURITY=none cannot send PAAS_SMTP_USER/PASSWORD in clear text; use starttls or tls")
	}
	if c.Port < 0 || c.Port > 65535 {
		return errors.New("PAAS_SMTP_PORT must be a TCP port")
	}
	return nil
}

func (c SMTPConfig) port() int {
	switch {
	case c.Port != 0:
		return c.Port
	case c.Security == SMTPTLS:
		return 465
	}
	return 587
}

func (c SMTPConfig) tlsConfig() *tls.Config {
	if c.TLSConfig != nil {
		cfg := c.TLSConfig.Clone()
		if cfg.ServerName == "" {
			cfg.ServerName = c.Host
		}
		return cfg
	}
	return &tls.Config{ServerName: c.Host, MinVersion: tls.VersionTLS12}
}

// EmailSender sends one message per recipient over a single SMTP session.
type EmailSender struct {
	Config SMTPConfig
	// Now is the Date header's clock (tests); nil is time.Now.
	Now func() time.Time
}

func (s EmailSender) Send(ctx context.Context, m Message) (Result, error) {
	cfg := s.Config
	if !cfg.Enabled() {
		return Result{}, permanent("SMTP sunucusu yapılandırılmamış (PAAS_SMTP_HOST)")
	}
	if len(m.Recipients) == 0 {
		return Result{}, permanent("alıcı yok")
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	addr := net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.port()))
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", addr)
	if err != nil {
		return Result{}, smtpError("bağlantı", err)
	}
	defer conn.Close()
	if dl, ok := ctx.Deadline(); ok {
		conn.SetDeadline(dl)
	}
	if cfg.Security == SMTPTLS {
		tc := tls.Client(conn, cfg.tlsConfig())
		if err := tc.HandshakeContext(ctx); err != nil {
			return Result{}, smtpError("TLS", err)
		}
		conn = tc
	}
	c, err := smtp.NewClient(conn, cfg.Host)
	if err != nil {
		return Result{}, smtpError("karşılama", err)
	}
	defer c.Close()
	local := cfg.LocalName
	if local == "" {
		local = "paas"
	}
	if err := c.Hello(local); err != nil {
		return Result{}, smtpError("EHLO", err)
	}
	if cfg.Security == "" || cfg.Security == SMTPStartTLS {
		if ok, _ := c.Extension("STARTTLS"); !ok {
			return Result{}, permanent("SMTP sunucusu STARTTLS desteklemiyor; e-posta şifresiz gönderilmez")
		}
		if err := c.StartTLS(cfg.tlsConfig()); err != nil {
			return Result{}, smtpError("STARTTLS", err)
		}
	}
	if cfg.User != "" {
		if ok, _ := c.Extension("AUTH"); !ok {
			return Result{}, permanent("SMTP sunucusu kimlik doğrulama (AUTH) sunmuyor")
		}
		if err := c.Auth(smtp.PlainAuth("", cfg.User, cfg.Password, cfg.Host)); err != nil {
			return Result{}, smtpError("kimlik doğrulama", err)
		}
	}
	from, _ := mail.ParseAddress(cfg.From)
	now := time.Now
	if s.Now != nil {
		now = s.Now
	}
	var rejected []string
	for _, to := range m.Recipients {
		msg, err := BuildEmail(from, to, m, now())
		if err != nil {
			rejected = append(rejected, to)
			continue
		}
		if err := c.Mail(from.Address); err != nil {
			return Result{}, smtpError("MAIL FROM", err)
		}
		if err := c.Rcpt(to); err != nil {
			var te *textproto.Error
			if errors.As(err, &te) && te.Code >= 500 {
				// This address is refused for good; the others still get it.
				rejected = append(rejected, to)
				c.Reset()
				continue
			}
			return Result{}, smtpError("RCPT TO", err)
		}
		w, err := c.Data()
		if err != nil {
			return Result{}, smtpError("DATA", err)
		}
		if _, err := w.Write(msg); err != nil {
			return Result{}, smtpError("DATA", err)
		}
		if err := w.Close(); err != nil {
			return Result{}, smtpError("DATA", err)
		}
	}
	c.Quit()
	if len(rejected) == len(m.Recipients) {
		return Result{}, permanent("sunucu tüm alıcıları reddetti: " + strings.Join(rejected, ", "))
	}
	res := Result{Code: 250}
	if len(rejected) > 0 {
		res.Note = "reddedilen alıcılar: " + strings.Join(rejected, ", ")
	}
	return res, nil
}

// smtpError classifies an SMTP failure: 5xx replies are permanent, 4xx
// replies and network errors are retried.
func smtpError(stage string, err error) error {
	var te *textproto.Error
	if errors.As(err, &te) {
		return &SendError{Msg: fmt.Sprintf("SMTP %s: %d %s", stage, te.Code, clip(oneLine(te.Msg), 160)), Permanent: te.Code >= 500}
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return &SendError{Msg: "SMTP " + stage + ": zaman aşımı"}
	}
	return &SendError{Msg: "SMTP " + stage + ": " + clip(oneLine(err.Error()), 160)}
}

// BuildEmail renders the RFC 5322 message for one recipient: UTF-8
// quoted-printable text, encoded subject, and a Message-ID derived from
// the idempotency key so a retried delivery is recognizable.
func BuildEmail(from *mail.Address, to string, m Message, now time.Time) ([]byte, error) {
	rcpt, err := mail.ParseAddress(to)
	if err != nil {
		return nil, err
	}
	domain := "paas"
	if i := strings.LastIndexByte(from.Address, '@'); i >= 0 {
		domain = from.Address[i+1:]
	}
	var b bytes.Buffer
	h := func(k, v string) {
		// Header values never carry line breaks (no header injection).
		b.WriteString(k + ": " + strings.NewReplacer("\r", " ", "\n", " ").Replace(v) + "\r\n")
	}
	h("From", (&mail.Address{Name: "paas", Address: from.Address}).String())
	h("To", rcpt.String())
	h("Subject", mime.QEncoding.Encode("utf-8", EmailSubject(m.Event)))
	h("Date", now.UTC().Format(time.RFC1123Z))
	h("Message-ID", "<"+m.IdempotencyKey+"@"+domain+">")
	h("MIME-Version", "1.0")
	h("Content-Type", "text/plain; charset=utf-8")
	h("Content-Transfer-Encoding", "quoted-printable")
	h("Auto-Submitted", "auto-generated")
	h("X-Paas-Event", m.Event.Type)
	if m.Event.ManageURL != "" {
		h("List-Unsubscribe", "<"+m.Event.ManageURL+">")
	}
	b.WriteString("\r\n")
	qp := quotedprintable.NewWriter(&b)
	body := strings.ReplaceAll(EmailBody(m.Event, m.Channel.Name), "\n", "\r\n")
	if _, err := qp.Write([]byte(body)); err != nil {
		return nil, err
	}
	if err := qp.Close(); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}
