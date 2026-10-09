package main

import (
	"context"
	"log/slog"
	"sync"

	"github.com/nisagwn/paas/internal/config"
	"github.com/nisagwn/paas/internal/github"
	"github.com/nisagwn/paas/internal/store"
)

// startCommentSync mirrors preview comment threads to pull requests (Faz 25)
// with the credentials of commit statuses: the GitHub App's installation
// tokens, else PAAS_GITHUB_TOKEN. It returns nil when GitHub statuses are
// off (PAAS_GITHUB_STATUS=false) or there are no credentials.
func startCommentSync(ctx context.Context, cfg config.Config, st *store.Store, repoTokens github.TokenSource,
	log *slog.Logger, wg *sync.WaitGroup) *github.CommentSync {
	if (cfg.GitHubToken == "" && repoTokens == nil) || !cfg.GitHubStatus {
		return nil
	}
	client := github.New(cfg.GitHubAPIURL, cfg.GitHubToken, log)
	client.Tokens = repoTokens // nil without the App: the token as before
	cs := &github.CommentSync{Client: client, Store: st, PublicURL: cfg.PublicURL, Log: log}
	wg.Add(1)
	go func() {
		defer wg.Done()
		cs.Run(ctx)
	}()
	log.Info("preview comments are mirrored to pull requests", "public_url", cfg.PublicURL)
	return cs
}
