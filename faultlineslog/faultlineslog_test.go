package faultlineslog_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	faultline "github.com/getfaultline/faultline-go"
	"github.com/getfaultline/faultline-go/faultlineslog"
	"github.com/getfaultline/faultline-go/internal/contracttest"
)

func setup(t *testing.T) *contracttest.Server {
	t.Helper()
	faultline.Close(time.Second)
	srv := contracttest.NewServer()
	if err := faultline.Init(faultline.Options{APIKey: "flt_ingest_test", Endpoint: srv.URL}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		faultline.Close(time.Second)
		srv.Close()
	})
	return srv
}

func flushed(t *testing.T, srv *contracttest.Server) []map[string]any {
	t.Helper()
	faultline.Flush(3 * time.Second)
	for _, r := range srv.Requests() {
		if err := contracttest.ValidateBatch(r.Body); err != nil {
			t.Fatal(err)
		}
	}
	return srv.Events()
}

func TestRecordsBecomeBreadcrumbs(t *testing.T) {
	srv := setup(t)
	var out bytes.Buffer
	next := slog.NewTextHandler(&out, &slog.HandlerOptions{Level: slog.LevelDebug})
	logger := slog.New(faultlineslog.NewHandler(next, nil))
	ctx := faultline.NewContext(context.Background())

	logger.DebugContext(ctx, "too verbose")
	logger.With("svc", "billing").WithGroup("req").InfoContext(ctx, "charging card", "amount", 42, "password", "hunter2")
	logger.WarnContext(ctx, "slow", slog.Group("db", "ms", 900))
	faultline.CaptureMessageContext(ctx, "done", "")

	ev := flushed(t, srv)[0]
	crumbs := ev["breadcrumbs"].([]any)
	if len(crumbs) != 2 {
		t.Fatalf("debug below BreadcrumbLevel must be skipped: %v", crumbs)
	}
	first := crumbs[0].(map[string]any)
	data := first["data"].(map[string]any)
	if first["type"] != "log" || first["category"] != "slog" || first["level"] != "info" || first["message"] != "charging card" {
		t.Fatalf("breadcrumb: %v", first)
	}
	if data["svc"] != "billing" || data["req.amount"] != float64(42) || data["req.password"] != "[Filtered]" {
		t.Fatalf("data: %v", data)
	}
	if second := crumbs[1].(map[string]any); second["level"] != "warning" || second["data"].(map[string]any)["db.ms"] != float64(900) {
		t.Fatalf("warn: %v", second)
	}
	if !strings.Contains(out.String(), "too verbose") || !strings.Contains(out.String(), "charging card") {
		t.Fatalf("records must still reach the next handler: %s", out.String())
	}
}

func TestErrorRecordsBecomeEvents(t *testing.T) {
	srv := setup(t)
	logger := slog.New(faultlineslog.NewHandler(nil, &faultlineslog.Options{EventLevel: slog.LevelError}))
	ctx := faultline.NewContext(context.Background())
	faultline.ScopeFromContext(ctx).SetTag("job", "sync")

	logger.ErrorContext(ctx, "sync failed", "err", errors.New("upstream timeout"))
	logger.ErrorContext(ctx, "plain error message")
	logger.InfoContext(ctx, "not an event")

	evs := flushed(t, srv)
	if len(evs) != 2 {
		t.Fatalf("events %d", len(evs))
	}
	ex := evs[0]["exception"].(map[string]any)
	if ex["value"] != "upstream timeout" || evs[0]["tags"].(map[string]any)["job"] != "sync" {
		t.Fatalf("exception event: %v", evs[0])
	}
	if evs[1]["message"] != "plain error message" || evs[1]["level"] != "error" || evs[1]["exception"] != nil {
		t.Fatalf("message event: %v", evs[1])
	}
}

func TestNoEventsByDefaultAndEnabled(t *testing.T) {
	srv := setup(t)
	h := faultlineslog.NewHandler(nil, nil)
	if h.Enabled(context.Background(), slog.LevelDebug) || !h.Enabled(context.Background(), slog.LevelInfo) {
		t.Fatal("Enabled")
	}
	slog.New(h).Error("only a breadcrumb")
	if n := len(flushed(t, srv)); n != 0 {
		t.Fatalf("events %d", n)
	}
	// nil context and a WithGroup("") must not panic
	var nilCtx context.Context // deliberately nil
	_ = h.WithGroup("").Handle(nilCtx, slog.NewRecord(time.Now(), slog.LevelInfo, "x", 0))
}
