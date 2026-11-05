package runtime

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

	transporthttp "github.com/arksnorman/open-kase/api/internal/transport/http"
	"github.com/arksnorman/open-kase/api/package/product"
)

const shutdownTimeout = 20 * time.Second

// serveOptions selects the in-process background work.
type serveOptions struct {
	Workers     bool // run the outbox dispatcher and job workers
	Concurrency int  // job worker pool size
}

// serve runs the HTTP server until ctx is cancelled, then drains in-flight
// requests and stops background loops.
func serve(ctx context.Context, c *Container, opts serveOptions) error {
	readiness := transporthttp.NewReadiness(c.Log,
		transporthttp.Check{Name: "mongodb", Required: true, Run: c.checkMongo},
		transporthttp.Check{Name: "masterKey", Required: true, Run: c.checkMasterKey},
		transporthttp.Check{Name: "settings", Required: true, Run: c.checkSettings},
		transporthttp.Check{Name: "redis", Required: false, Run: c.checkRedis},
	)
	router, err := transporthttp.NewRouter(transporthttp.Deps{
		Logger: c.Log, Readiness: readiness, TrustedProxies: c.Boot.trustedProxies(),
		Services: c.httpServices(), UIDir: c.Boot.UIDir, MCP: c.mcpHandler(),
	})
	if err != nil {
		return fmt.Errorf("router: %w", err)
	}
	srv := &http.Server{
		Addr:              c.Boot.HTTPAddr,
		Handler:           router,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    64 << 10,
		BaseContext:       func(net.Listener) context.Context { return context.WithoutCancel(ctx) },
	}
	ln, err := net.Listen("tcp", c.Boot.HTTPAddr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", c.Boot.HTTPAddr, err)
	}

	bgCtx, stopBackground := context.WithCancel(ctx)
	var bg sync.WaitGroup
	bg.Go(func() { c.watchSettings(bgCtx) })
	bg.Go(func() { c.cleanupUploads(bgCtx) })
	bg.Go(func() { c.listenLiveHints(bgCtx) })
	// Live update streams end on shutdown instead of holding it open.
	srv.RegisterOnShutdown(c.Live.Close)
	if opts.Workers {
		bg.Go(func() { c.runWorkers(bgCtx, "serve", opts.Concurrency) })
	}

	if rep := readiness.Evaluate(ctx); !rep.Ready {
		c.Log.Warn("server starting but not ready", "checks", rep.Checks)
	}
	c.Log.Info("http server listening", "addr", ln.Addr().String(), "env", c.Boot.Env, "version", product.Version)

	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(ln) }()

	select {
	case err := <-errCh:
		stopBackground()
		bg.Wait()
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
	}

	c.Log.Info("shutdown requested; draining requests", "timeout", shutdownTimeout.String())
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
	defer cancel()
	err = srv.Shutdown(shutdownCtx)
	stopBackground()
	bg.Wait()
	if err != nil {
		return fmt.Errorf("graceful shutdown: %w", err)
	}
	c.Log.Info("http server stopped")
	return nil
}
