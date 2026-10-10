package faultline_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	faultline "github.com/getfaultline/faultline-go"
)

func TestContextScopesDoNotLeakAcrossGoroutines(t *testing.T) {
	srv := setup(t, faultline.Options{})
	faultline.SetTag("global", "yes")
	const n = 40
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ctx := faultline.NewContext(context.Background())
			scope := faultline.ScopeFromContext(ctx)
			id := fmt.Sprint(i)
			scope.SetTag("worker", id)
			scope.SetUser(faultline.User{ID: "user-" + id})
			faultline.AddBreadcrumbContext(ctx, faultline.Breadcrumb{Message: "step-" + id})
			child := faultline.NewContext(ctx) // forks inherit, but do not write back
			faultline.ScopeFromContext(child).SetTag("child", id)
			faultline.CaptureExceptionContext(ctx, fmt.Errorf("failure in worker %s", id))
		}(i)
	}
	wg.Wait()
	events := flush(t, srv)
	if len(events) != n {
		t.Fatalf("events %d", len(events))
	}
	for _, ev := range events {
		tags := tagsOf(ev)
		id := tags["worker"].(string)
		if ev["message"] != "failure in worker "+id || ev["user"].(map[string]any)["id"] != "user-"+id {
			t.Fatalf("scope leaked: %v", ev)
		}
		if tags["global"] != "yes" || tags["child"] != nil {
			t.Fatalf("inheritance: %v", tags)
		}
		crumbs := ev["breadcrumbs"].([]any)
		if len(crumbs) != 1 || crumbs[0].(map[string]any)["message"] != "step-"+id {
			t.Fatalf("breadcrumbs leaked: %v", crumbs)
		}
	}
	faultline.CaptureMessage("after", "")
	last := flush(t, srv)
	if tags := tagsOf(last[len(last)-1]); tags["worker"] != nil {
		t.Fatalf("global scope polluted: %v", tags)
	}
}

func TestWithScope(t *testing.T) {
	srv := setup(t, faultline.Options{})
	faultline.WithScope(func(s *faultline.Scope) {
		s.SetTag("inside", "1")
		s.SetLevel(faultline.LevelWarning)
		s.SetFingerprint([]string{"custom"})
		faultline.CaptureException(errors.New("package function sees the fork"))
		s.SetFingerprint(nil) // same fingerprint within 1s would be deduped
		s.CaptureMessage("scope method", "")
	})
	faultline.CaptureMessage("outside", "")
	events := flush(t, srv)
	if len(events) != 3 {
		t.Fatalf("events %d", len(events))
	}
	if tagsOf(events[0])["inside"] != "1" || events[0]["level"] != "warning" || events[0]["fingerprint"].([]any)[0] != "custom" {
		t.Fatalf("inside: %v", events[0])
	}
	if events[1]["level"] != "warning" || tagsOf(events[1])["inside"] != "1" {
		t.Fatalf("scope method: %v", events[1])
	}
	if tagsOf(events[2])["inside"] != nil || events[2]["level"] != "info" {
		t.Fatalf("outside must not see the fork: %v", events[2])
	}
}

func TestWithScopePopsOnPanic(t *testing.T) {
	srv := setup(t, faultline.Options{})
	runRecovered(func() {
		faultline.WithScope(func(s *faultline.Scope) {
			s.SetTag("leak", "1")
			panic("user code")
		})
	})
	faultline.CaptureMessage("after", "")
	if tagsOf(only(t, flush(t, srv)))["leak"] != nil {
		t.Fatal("scope not popped after panic")
	}
}

func TestScopeFromContextWithoutScope(t *testing.T) {
	if faultline.ScopeFromContext(context.Background()) != nil {
		t.Fatal("expected nil")
	}
	var s *faultline.Scope
	s.SetTag("a", "b") // nil-safe
	s.AddBreadcrumb(faultline.Breadcrumb{})
	s.Clear()
}
