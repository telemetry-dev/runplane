package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/runplane/runplane/internal/config"
	"github.com/runplane/runplane/internal/host"
)

// serveServicePort serves the service port of the sandboxes on all
// addresses: /v2/ goes to the registry mirror, all other paths to the
// Actions cache. The returned function stops the listener.
func serveServicePort(cfg config.Config, mirror, cache http.Handler, log *slog.Logger) (func(), error) {
	ln, err := net.Listen("tcp", servicePortAddr(cfg))
	if err != nil {
		return nil, fmt.Errorf("listen on the service port: %w", err)
	}
	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/v2" || strings.HasPrefix(r.URL.Path, "/v2/") {
				mirror.ServeHTTP(w, r)
				return
			}
			cache.ServeHTTP(w, r)
		}),
		// Cache blobs can be several GB, so the requests have no read or
		// write time limit.
		ReadHeaderTimeout: 10 * time.Second,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelDebug),
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
			log.Error("service port", "err", err)
		}
	}()
	return func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			srv.Close()
		}
		<-done
	}, nil
}

func servicePortAddr(cfg config.Config) string { return fmt.Sprintf(":%d", cfg.ServicePort) }

// mirrorLimit is the size limit of the registry mirror: a tenth of the data
// disk, from 2 to 50 GiB.
func mirrorLimit(dataDir string) int64 {
	res, err := host.ReadResources(dataDir)
	if err != nil {
		return 10 << 30
	}
	return min(max(res.DiskBytes/10, 2<<30), 50<<30)
}
