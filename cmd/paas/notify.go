package main

import (
	"context"
	"log/slog"
	"sync"

	"github.com/nisagwn/paas/internal/config"
	"github.com/nisagwn/paas/internal/notify"
	"github.com/nisagwn/paas/internal/store"
	"github.com/nisagwn/paas/internal/worker"
)

// startNotifications runs the notification dispatcher (Faz 26) and returns
// the service producers emit events through. It runs on every replica;
// deliveries are claimed with FOR UPDATE SKIP LOCKED and LISTEN wakes
// every dispatcher when one is queued.
func startNotifications(ctx context.Context, cfg config.Config, st *store.Store, log *slog.Logger, wg *sync.WaitGroup) (*notify.Service, error) {
	nc, err := config.LoadNotify()
	if err != nil {
		return nil, err
	}
	smtp := notify.SMTPConfig{
		Host: nc.SMTPHost, Port: nc.SMTPPort, User: nc.SMTPUser, Password: nc.SMTPPassword,
		From: nc.SMTPFrom, Security: nc.SMTPSecurity,
	}
	if err := smtp.Check(); err != nil {
		return nil, err
	}
	client := notify.NewHTTPClient(0, nc.AllowPrivate)
	d := &notify.Dispatcher{
		Store: st, Log: log, Interval: nc.Interval, MaxAttempts: nc.MaxAttempts, RateLimit: nc.RateLimit,
		ListenURL: cfg.DatabaseURL,
		Senders: map[string]notify.Sender{
			store.ChannelEmail:   notify.EmailSender{Config: smtp},
			store.ChannelSlack:   notify.SlackSender{Client: client},
			store.ChannelDiscord: notify.DiscordSender{Client: client},
			store.ChannelWebhook: notify.WebhookSender{Client: client},
		},
	}
	svc := &notify.Service{
		Store: st, PublicURL: cfg.PublicURL, Dispatcher: d, SMTPConfigured: smtp.Enabled(), Log: log,
		Validation: notify.Validation{AllowInsecureURLs: nc.AllowPrivate},
	}
	log.Info("notifications enabled", "smtp", smtp.Enabled(), "rate_limit", nc.RateLimit,
		"max_attempts", nc.MaxAttempts, "allow_private", nc.AllowPrivate)
	wg.Add(1)
	go func() {
		defer wg.Done()
		d.Run(ctx)
	}()
	return svc, nil
}

// deploymentNotifier chains the event notifier before n (GitHub).
func deploymentNotifier(svc *notify.Service, st *store.Store, log *slog.Logger, n worker.Notifier) worker.Notifier {
	return notify.Chain(&notify.DeploymentEvents{Emitter: svc, Store: st, Log: log}, n)
}
