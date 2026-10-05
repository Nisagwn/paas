// Command paas-cli is the `paas` command-line client (Faz 18), in the
// spirit of the `vercel` CLI. Build it as `paas`:
//
//	go build -o paas ./cmd/paas-cli
//
// The server binary lives in cmd/paas; this is the client for its API.
package main

import (
	"context"
	"os"
	"os/signal"

	"github.com/nisagwn/paas/internal/cli"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	code := cli.New().Run(ctx, os.Args[1:])
	stop()
	os.Exit(code)
}
