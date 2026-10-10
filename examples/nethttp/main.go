// Command nethttp is a small net/http server instrumented with Faultline.
//
//	FAULTLINE_API_KEY=flt_ingest_... FAULTLINE_ENDPOINT=http://localhost:4000 go run ./examples/nethttp
//
// Then visit http://localhost:3002/ for the list of routes. Without FAULTLINE_API_KEY the SDK
// stays disabled and no events are sent.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"time"

	faultline "github.com/getfaultline/faultline-go"
	"github.com/getfaultline/faultline-go/faultlinehttp"
	"github.com/getfaultline/faultline-go/faultlineslog"
)

var errOutOfStock = errors.New("item out of stock")

func main() {
	apiKey := os.Getenv("FAULTLINE_API_KEY")
	endpoint := os.Getenv("FAULTLINE_ENDPOINT")
	if endpoint == "" {
		endpoint = "http://localhost:4000"
	}
	port := os.Getenv("PORT")
	if port == "" {
		port = "3002"
	}
	if apiKey == "" {
		log.Println("FAULTLINE_API_KEY is not set; the SDK stays disabled and no events are sent.")
	}

	// 1. Initialize as early as possible.
	if err := faultline.Init(faultline.Options{
		APIKey:      apiKey,
		Endpoint:    endpoint,
		Environment: "development",
		Release:     "example-nethttp@1.0.0",
		Debug:       os.Getenv("FAULTLINE_DEBUG") == "1",
		Enabled:     faultline.Bool(apiKey != ""),
	}); err != nil {
		log.Printf("faultline: %v", err)
	}
	// 2. Deliver queued events before exiting.
	defer faultline.Close(2 * time.Second)

	// 3. slog records become breadcrumbs; errors also become events.
	logger := slog.New(faultlineslog.NewHandler(slog.NewTextHandler(os.Stderr, nil), &faultlineslog.Options{
		EventLevel: slog.LevelError,
	}))

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"ok": true, "routes": []string{"/boom", "/wrapped", "/message", "/log-error", "/status"}})
	})
	// A nil-pointer panic: recovered and captured by the middleware (level fatal).
	mux.HandleFunc("/boom", func(w http.ResponseWriter, r *http.Request) {
		faultline.ScopeFromContext(r.Context()).SetTag("route", "boom")
		var cfg *struct{ Name string }
		fmt.Fprint(w, cfg.Name)
	})
	// A wrapped error with a stack from where it was created, plus user and breadcrumbs.
	mux.HandleFunc("/wrapped", func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		scope := faultline.ScopeFromContext(ctx)
		scope.SetUser(faultline.User{ID: "user-123", Email: "jane@example.com"})
		scope.AddBreadcrumb(faultline.Breadcrumb{Category: "cart", Message: "checkout started"})
		if err := checkout(ctx, 7); err != nil {
			id := faultline.CaptureExceptionContext(ctx, err)
			w.WriteHeader(http.StatusConflict)
			writeJSON(w, map[string]any{"error": err.Error(), "eventId": id})
		}
	})
	mux.HandleFunc("/message", func(w http.ResponseWriter, r *http.Request) {
		id := faultline.CaptureMessageContext(r.Context(), "Payment webhook retried", faultline.LevelWarning)
		writeJSON(w, map[string]any{"eventId": id})
	})
	mux.HandleFunc("/log-error", func(w http.ResponseWriter, r *http.Request) {
		logger.InfoContext(r.Context(), "syncing inventory", "batch", 3)
		logger.ErrorContext(r.Context(), "inventory sync failed", "err", errors.New("upstream timeout"))
		writeJSON(w, map[string]any{"logged": true})
	})
	// A 5xx response without a panic is reported as a message event.
	mux.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		code, _ := strconv.Atoi(r.URL.Query().Get("code"))
		if code < 100 || code > 599 {
			code = http.StatusServiceUnavailable
		}
		w.WriteHeader(code)
	})

	srv := &http.Server{
		Addr:              ":" + port,
		Handler:           faultlinehttp.New(faultlinehttp.Options{}).Handle(mux),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		log.Printf("listening on http://localhost:%s (events go to %s)", port, endpoint)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatal(err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt)
	<-stop
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
}

func checkout(ctx context.Context, itemID int) error {
	if err := reserve(itemID); err != nil {
		return fmt.Errorf("checkout item %d: %w", itemID, err)
	}
	return nil
}

func reserve(int) error {
	return faultline.WithStack(errOutOfStock)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
