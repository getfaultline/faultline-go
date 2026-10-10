package faultline_test

import (
	"context"
	"math"
	"net/http"
	"testing"
	"time"

	faultline "github.com/getfaultline/faultline-go"
)

type nilReceiverErr struct{ msg *string }

func (e *nilReceiverErr) Error() string { return *e.msg } // panics on nil receiver

type panickyStringer struct{}

func (panickyStringer) String() string { panic("stringer") }

type badMarshal struct{}

func (badMarshal) MarshalJSON() ([]byte, error) { panic("marshal") }

func exerciseAll(t *testing.T) {
	t.Helper()
	var nilCtx context.Context // deliberately nil
	var typedNil *nilReceiverErr
	var nilScope *faultline.Scope
	cyc := map[string]any{}
	cyc["self"] = cyc

	faultline.CaptureException(nil)
	faultline.CaptureException(typedNil)
	faultline.CaptureExceptionContext(nilCtx, typedNil)
	faultline.CaptureMessage("", "bogus-level")
	faultline.CaptureMessageContext(nilCtx, "m", "")
	faultline.CapturePanic(nilCtx, panickyStringer{})
	faultline.SetUser(faultline.User{})
	faultline.SetTag("", "")
	faultline.SetTags(nil)
	faultline.SetContext("x", nil)
	faultline.SetContext("y", map[string]any{"cyc": cyc, "f": func() {}, "nan": math.NaN(), "bad": badMarshal{}})
	faultline.SetExtra("ch", make(chan int))
	faultline.SetExtra("bad", badMarshal{})
	faultline.AddBreadcrumb(faultline.Breadcrumb{Type: "nope", Level: "nope", Data: map[string]any{"cyc": cyc}})
	faultline.AddBreadcrumbContext(nilCtx, faultline.Breadcrumb{})
	faultline.WithScope(nil)
	faultline.WithScope(func(s *faultline.Scope) { s.SetExtra("k", badMarshal{}) })
	ctx := faultline.NewContext(nilCtx)
	faultline.ScopeFromContext(ctx).SetHTTPRequest(nil)
	faultline.ScopeFromContext(ctx).SetHTTPRequest(&http.Request{})
	faultline.ScopeFromContext(nilCtx).SetTag("a", "b")
	nilScope.CaptureException(typedNil)
	nilScope.CaptureMessage("m", "")
	faultline.CaptureException(&nilReceiverErr{})
	_ = faultline.WithStack(typedNil)
	faultline.Flush(0)
	faultline.Flush(-time.Second)
}

func TestNeverPanicsBeforeInit(t *testing.T) {
	faultline.ResetForTest()
	exerciseAll(t)
	faultline.Close(0)
}

func TestNeverPanicsWhileRunningAndAfterClose(t *testing.T) {
	srv := setup(t, faultline.Options{
		Debug: false,
		BeforeSend: func(e *faultline.Event) *faultline.Event {
			e.Extra = map[string]any{"hostile": make(chan int)}
			e.Level = "invalid"
			e.Exception = nil
			return e
		},
	})
	exerciseAll(t)
	for _, ev := range flush(t, srv) { // flush validates every payload against the schema
		if ev["level"] != "error" {
			t.Fatalf("BeforeSend output must be re-sanitized: %v", ev["level"])
		}
	}
	faultline.Close(time.Second)
	exerciseAll(t)
	faultline.Close(time.Second)
}

func TestNeverPanicsWithUnreachableEndpoint(t *testing.T) {
	faultline.ResetForTest()
	defer faultline.ResetForTest()
	if err := faultline.Init(faultline.Options{APIKey: "k", Endpoint: "http://127.0.0.1:1"}); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	faultline.CaptureMessage("lost", "")
	if time.Since(start) > 100*time.Millisecond {
		t.Fatal("capture must not block on the network")
	}
	faultline.Close(200 * time.Millisecond)
}
