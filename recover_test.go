package faultline_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	faultline "github.com/getfaultline/faultline-go"
)

//go:noinline
func explode() { panic("kaboom") }

//go:noinline
func nilDeref() int {
	var p *struct{ n int }
	return p.n
}

func runRecovered(fn func()) (repanicked any) {
	defer func() { repanicked = recover() }()
	fn()
	return nil
}

func TestRecoverRepanicsByDefault(t *testing.T) {
	srv := setup(t, faultline.Options{})
	got := runRecovered(func() {
		defer faultline.Recover()
		explode()
	})
	if got != "kaboom" {
		t.Fatalf("Recover must re-panic with the original value, got %v", got)
	}
	ev := only(t, flush(t, srv))
	ex := exceptionOf(ev)
	if ev["level"] != "fatal" || ex["type"] != "panic" || ex["value"] != "kaboom" {
		t.Fatalf("event: %v", ev)
	}
	if top := newest(ex); top["function"] != "faultline-go_test.explode" {
		t.Fatalf("newest frame should be the panic site: %v", top)
	}
	for _, f := range framesOf(ex) {
		if f["function"] == "runtime.gopanic" || f["function"] == "faultline.Recover" {
			t.Fatalf("recovery machinery leaked into frames: %v", f)
		}
	}
}

func TestRecoverSwallowsWhenConfigured(t *testing.T) {
	srv := setup(t, faultline.Options{SwallowPanics: true})
	got := runRecovered(func() {
		defer faultline.Recover()
		_ = nilDeref()
	})
	if got != nil {
		t.Fatalf("panic should be swallowed, got %v", got)
	}
	ex := exceptionOf(only(t, flush(t, srv)))
	if typ, _ := ex["type"].(string); !strings.HasPrefix(typ, "runtime.") {
		t.Fatalf("runtime errors keep their concrete type: %v", ex["type"])
	}
	if top := newest(ex); top["function"] != "faultline-go_test.nilDeref" {
		t.Fatalf("newest in-app frame: %v", top)
	}
}

func TestRecoverWithContextUsesRequestScope(t *testing.T) {
	srv := setup(t, faultline.Options{SwallowPanics: true})
	ctx := faultline.NewContext(context.Background())
	faultline.ScopeFromContext(ctx).SetTag("request", "r1")
	runRecovered(func() {
		defer faultline.RecoverWithContext(ctx)
		panic(errors.New("typed"))
	})
	ev := only(t, flush(t, srv))
	if tagsOf(ev)["request"] != "r1" || exceptionOf(ev)["type"] != "*errors.errorString" {
		t.Fatalf("event: %v", ev)
	}
}

func TestRecoverWithoutInitStillRepanics(t *testing.T) {
	faultline.ResetForTest()
	got := runRecovered(func() {
		defer faultline.Recover()
		panic("no sdk")
	})
	if got != "no sdk" {
		t.Fatalf("got %v", got)
	}
	if runRecovered(func() { defer faultline.Recover() }) != nil {
		t.Fatal("no panic, nothing to do")
	}
}

func TestCapturePanicFromMiddlewareStyleDefer(t *testing.T) {
	srv := setup(t, faultline.Options{})
	func() {
		defer func() {
			if v := recover(); v != nil {
				faultline.CapturePanic(context.Background(), v)
			}
		}()
		explode()
	}()
	ex := exceptionOf(only(t, flush(t, srv)))
	if newest(ex)["function"] != "faultline-go_test.explode" {
		t.Fatalf("frame: %v", newest(ex))
	}
	if faultline.CapturePanic(context.Background(), nil) != nil {
		t.Fatal("nil value")
	}
}
