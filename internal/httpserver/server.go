// Package httpserver exposes health and read-only status endpoints.
// There is no UI and no ingress — job discovery is pull-only.
package httpserver

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/Lakr233/mini-control-action/internal/config"
	"github.com/Lakr233/mini-control-action/internal/state"
)

func New(cfg *config.Config, store *state.Store, log *slog.Logger, details ...func() any) *http.Server {
	mux := http.NewServeMux()

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})

	mux.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		recs := store.List()
		active := 0
		for _, rec := range recs {
			if !rec.State.Terminal() {
				active++
			}
		}
		w.Header().Set("Content-Type", "application/json")
		response := map[string]any{
			"active_jobs": active,
			"records":     recs,
		}
		if len(details) > 0 {
			response["scheduling"] = details[0]()
		}
		_ = json.NewEncoder(w).Encode(response)
	})

	return &http.Server{
		Addr:              cfg.Server.Listen,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
}

// Run serves until ctx is cancelled, then shuts down gracefully.
func Run(ctx context.Context, srv *http.Server, log *slog.Logger) error {
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return srv.Shutdown(sctx)
	}
}
