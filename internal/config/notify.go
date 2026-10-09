package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Notify configures notifications (Faz 26, internal/notify). Channels and
// their URLs live in the database; this is the SMTP server for email
// channels and the dispatcher's tuning.
type Notify struct {
	// SMTP (email channels): PAAS_SMTP_HOST, PAAS_SMTP_PORT (587; 465 with
	// tls), PAAS_SMTP_USER, PAAS_SMTP_PASSWORD, PAAS_SMTP_FROM and
	// PAAS_SMTP_SECURITY: starttls (default, required by the server),
	// tls (implicit, port 465) or none (development relays, no AUTH).
	SMTPHost, SMTPUser, SMTPPassword, SMTPFrom, SMTPSecurity string
	SMTPPort                                                 int

	// PAAS_NOTIFY_INTERVAL: how often the outbox is polled besides
	// LISTEN/NOTIFY wake-ups (default 5s, at least 1s).
	Interval time.Duration
	// PAAS_NOTIFY_MAX_ATTEMPTS (default 8, 1–20).
	MaxAttempts int
	// PAAS_NOTIFY_RATE_LIMIT: messages per channel and minute (default 30, 0 = unlimited).
	RateLimit int
	// PAAS_NOTIFY_ALLOW_PRIVATE=true lets webhooks reach private and
	// loopback addresses and accepts http:// URLs (development only).
	AllowPrivate bool
}

// LoadNotify reads the notification settings.
func LoadNotify() (Notify, error) {
	n := Notify{
		SMTPHost: strings.TrimSpace(os.Getenv("PAAS_SMTP_HOST")), SMTPUser: os.Getenv("PAAS_SMTP_USER"),
		SMTPPassword: os.Getenv("PAAS_SMTP_PASSWORD"), SMTPFrom: strings.TrimSpace(os.Getenv("PAAS_SMTP_FROM")),
		SMTPSecurity: strings.ToLower(strings.TrimSpace(os.Getenv("PAAS_SMTP_SECURITY"))),
		Interval:     5 * time.Second, MaxAttempts: 8, RateLimit: 30,
		AllowPrivate: os.Getenv("PAAS_NOTIFY_ALLOW_PRIVATE") == "true",
	}
	if v := os.Getenv("PAAS_SMTP_PORT"); v != "" {
		p, err := strconv.Atoi(v)
		if err != nil || p < 1 || p > 65535 {
			return n, fmt.Errorf("PAAS_SMTP_PORT must be a TCP port")
		}
		n.SMTPPort = p
	}
	if n.SMTPHost != "" && n.SMTPFrom == "" {
		return n, fmt.Errorf("PAAS_SMTP_FROM is required with PAAS_SMTP_HOST (the sender address, e.g. paas@example.com)")
	}
	if v := os.Getenv("PAAS_NOTIFY_INTERVAL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d < time.Second {
			return n, fmt.Errorf("PAAS_NOTIFY_INTERVAL must be a duration of at least 1s (e.g. 5s)")
		}
		n.Interval = d
	}
	if v := os.Getenv("PAAS_NOTIFY_MAX_ATTEMPTS"); v != "" {
		a, err := strconv.Atoi(v)
		if err != nil || a < 1 || a > 20 {
			return n, fmt.Errorf("PAAS_NOTIFY_MAX_ATTEMPTS must be between 1 and 20")
		}
		n.MaxAttempts = a
	}
	if v := os.Getenv("PAAS_NOTIFY_RATE_LIMIT"); v != "" {
		r, err := strconv.Atoi(v)
		if err != nil || r < 0 {
			return n, fmt.Errorf("PAAS_NOTIFY_RATE_LIMIT must be a number of messages per minute (0 = unlimited)")
		}
		n.RateLimit = r
	}
	return n, nil
}
