package faultline_test

import (
	"testing"
	"time"

	faultline "github.com/getfaultline/faultline-go"
	"github.com/getfaultline/faultline-go/internal/contracttest"
)

func setup(t *testing.T, opts faultline.Options) *contracttest.Server {
	t.Helper()
	faultline.ResetForTest()
	srv := contracttest.NewServer()
	if opts.APIKey == "" {
		opts.APIKey = "flt_ingest_test"
	}
	opts.Endpoint = srv.URL
	if err := faultline.Init(opts); err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(func() {
		faultline.ResetForTest()
		srv.Close()
	})
	return srv
}

// flush delivers queued events, validates every request body against the contract schema and
// returns all events received so far.
func flush(t *testing.T, srv *contracttest.Server) []map[string]any {
	t.Helper()
	if !faultline.Flush(3 * time.Second) {
		t.Fatal("Flush timed out")
	}
	for _, r := range srv.Requests() {
		if err := contracttest.ValidateBatch(r.Body); err != nil {
			t.Fatal(err)
		}
	}
	return srv.Events()
}

func only(t *testing.T, events []map[string]any) map[string]any {
	t.Helper()
	if len(events) != 1 {
		t.Fatalf("expected exactly 1 event, got %d", len(events))
	}
	return events[0]
}

func exceptionOf(ev map[string]any) map[string]any {
	ex, _ := ev["exception"].(map[string]any)
	return ex
}

func chainOf(ev map[string]any) []map[string]any {
	var out []map[string]any
	for ex := exceptionOf(ev); ex != nil; ex, _ = ex["cause"].(map[string]any) {
		out = append(out, ex)
	}
	return out
}

func framesOf(ex map[string]any) []map[string]any {
	st, _ := ex["stacktrace"].(map[string]any)
	raw, _ := st["frames"].([]any)
	out := make([]map[string]any, len(raw))
	for i, f := range raw {
		out[i], _ = f.(map[string]any)
	}
	return out
}

func newest(ex map[string]any) map[string]any {
	f := framesOf(ex)
	if len(f) == 0 {
		return nil
	}
	return f[len(f)-1]
}

func tagsOf(ev map[string]any) map[string]any {
	tags, _ := ev["tags"].(map[string]any)
	return tags
}
