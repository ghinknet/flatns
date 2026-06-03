// Command flatns is a CNAME/NS flattening daemon. It resolves a CNAME chain to
// its terminal A/AAAA records and keeps a target subdomain's records in sync
// with the result, working around DNS providers that gate native flattening
// behind paid plans. Each flatten entry is reconciled by its own goroutine, and
// the configuration hot-reloads on SIGHUP.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"flatns/internal/flattener"
	"flatns/internal/infra/config"
	"flatns/internal/infra/logger"

	"go.uber.org/zap"

	// Side-effect imports register the available provider implementations with
	// the provider registry. Adding a new vendor is as simple as adding another
	// blank import here once its package is implemented.
	_ "flatns/internal/provider/aliyun"
	_ "flatns/internal/provider/tencent"
)

func main() {
	// Load config; the package owns its own SIGHUP-driven reload goroutine.
	config.Init()
	defer config.Cleanup()

	// Init logger and rebuild it on every config reload.
	logger.Init()
	defer logger.Cleanup()
	config.OnReload(logger.Reload)

	logger.L.Info("flatns starting")

	// Spawn the per-entry reconcile workers; they rebuild themselves on reload.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	flattener.Start(ctx)
	defer flattener.Cleanup()

	// Block until a shutdown signal arrives; deferred cleanups then unwind.
	waitForShutdown()
}

// waitForShutdown blocks until SIGINT or SIGTERM is received.
func waitForShutdown() {
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	sig := <-sigChan
	logger.L.Info("received shutdown signal, stopping", zap.String("signal", sig.String()))
}
