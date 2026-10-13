package ghsource

import (
	"context"
	"io"
	"log"
	"net/http"
	"time"
)

// maxWebhookBody caps a webhook payload (GitHub's documented limit is 25 MB).
const maxWebhookBody = 25 << 20

// webhookHandler builds the HTTP handler for direct (vanilla) GitHub webhooks.
// Unlike smee, it sees the exact raw body, so signature verification is
// reliable — keep verify_signature on for this transport.
func (g *Source) webhookHandler(ctx context.Context, emit EmitFunc, seen *deliveryDedup) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxWebhookBody))
		if err != nil {
			http.Error(w, "read body", http.StatusBadRequest)
			return
		}
		event := r.Header.Get("X-GitHub-Event")
		if event == "" {
			http.Error(w, "missing X-GitHub-Event", http.StatusBadRequest)
			return
		}
		if event == "ping" {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("pong"))
			return
		}
		g.deliver(ctx, emit, seen,
			event, r.Header.Get("X-GitHub-Delivery"), r.Header.Get("X-Hub-Signature-256"), body)
		w.WriteHeader(http.StatusAccepted)
	}
}

// serveHTTP runs the direct webhook receiver until ctx is cancelled.
func (g *Source) serveHTTP(ctx context.Context, emit EmitFunc, seen *deliveryDedup) error {
	path := g.cfg.Webhook.Path
	if path == "" {
		path = "/webhook"
	}
	mux := http.NewServeMux()
	mux.HandleFunc(path, g.webhookHandler(ctx, emit, seen))
	srv := &http.Server{Addr: g.cfg.Webhook.Listen, Handler: mux}

	go func() {
		<-ctx.Done()
		sd, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sd)
	}()

	log.Printf("github[%s]: webhook listener on %s%s", g.name, g.cfg.Webhook.Listen, path)
	if g.cfg.Webhook.PublicURL != "" {
		// The engine opened webhook.expose for us (the `listeners`
		// connection semantic, plugin-contract.md §2.4) and handed back
		// this URL — point GitHub's webhook delivery at it yourself;
		// registering it with GitHub is not this plugin's job.
		log.Printf("github[%s]: webhook listener %s is reachable at %s", g.name, g.cfg.Webhook.Listen, g.cfg.Webhook.PublicURL)
	}
	err := srv.ListenAndServe()
	if err == http.ErrServerClosed {
		return ctx.Err()
	}
	return err
}
