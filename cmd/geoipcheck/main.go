// Command geoipcheck serves country checks over HTTP and gRPC. All
// configuration is GEOIPCHECK_* environment variables; see README.md.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/enottons/geoipcheck/internal/app"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	// After the first signal, restore default handling so a second one
	// kills the process instead of waiting out the drain.
	go func() {
		<-ctx.Done()
		stop()
	}()
	os.Exit(app.Run(ctx, os.Getenv, os.Stdout))
}
