package faultline_test

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	faultline "github.com/getfaultline/faultline-go"
)

type Server struct{}

//go:noinline
func (s *Server) handle() bool {
	return faultline.CaptureException(errors.New("handler failed")) == nil
}

func TestCaptureExceptionPayload(t *testing.T) {
	srv := setup(t, faultline.Options{Environment: "test", Release: "api@1.0.0", ServerName: "web-1"})
	faultline.SetTag("region", "eu")
	faultline.SetUser(faultline.User{ID: "u1", Email: "a@b.c"})
	faultline.SetContext("order", map[string]any{"id": 42})
	faultline.SetExtra("attempt", 3)
	faultline.AddBreadcrumb(faultline.Breadcrumb{Message: "clicked", Category: "ui"})

	id := faultline.CaptureException(errors.New("boom"))
	if id == nil {
		t.Fatal("expected an event id")
	}
	ev := only(t, flush(t, srv))
	if ev["eventId"] != string(*id) || ev["platform"] != "go" || ev["level"] != "error" || ev["message"] != "boom" {
		t.Fatalf("event: %v", ev)
	}
	if ev["environment"] != "test" || ev["release"] != "api@1.0.0" || ev["serverName"] != "web-1" {
		t.Fatalf("options: %v", ev)
	}
	sdk := ev["sdk"].(map[string]any)
	if sdk["name"] != "faultline-go" || sdk["version"] != faultline.SDKVersion {
		t.Fatalf("sdk: %v", sdk)
	}
	rt := ev["contexts"].(map[string]any)["runtime"].(map[string]any)
	if rt["name"] != "go" || rt["version"] == "" {
		t.Fatalf("runtime: %v", rt)
	}
	if ev["contexts"].(map[string]any)["order"] == nil || tagsOf(ev)["region"] != "eu" || ev["user"].(map[string]any)["id"] != "u1" {
		t.Fatalf("scope data missing: %v", ev)
	}
	if crumbs := ev["breadcrumbs"].([]any); len(crumbs) != 1 {
		t.Fatalf("breadcrumbs: %v", crumbs)
	}
	ex := exceptionOf(ev)
	if ex["type"] != "*errors.errorString" || ex["value"] != "boom" {
		t.Fatalf("exception: %v", ex)
	}
	top := newest(ex)
	if top["function"] != "faultline-go_test.TestCaptureExceptionPayload" || top["inApp"] != true || !strings.HasSuffix(top["filename"].(string), "capture_test.go") {
		t.Fatalf("newest frame should be the call site: %v", top)
	}
	if !strings.Contains(top["contextLine"].(string), "faultline.CaptureException(errors.New(\"boom\"))") {
		t.Fatalf("source context: %v", top["contextLine"])
	}
	if len(top["preContext"].([]any)) != 5 || len(top["postContext"].([]any)) != 5 {
		t.Fatalf("pre/post context: %v", top)
	}
	frames := framesOf(ex)
	if frames[0]["inApp"] != false {
		t.Fatalf("oldest frame (runtime/testing) should not be in-app: %v", frames[0])
	}
}

func TestMethodFrameNames(t *testing.T) {
	srv := setup(t, faultline.Options{})
	(&Server{}).handle()
	ex := exceptionOf(only(t, flush(t, srv)))
	top := newest(ex)
	if top["function"] != "faultline-go_test.(*Server).handle" || top["module"] != "github.com/getfaultline/faultline-go_test" {
		t.Fatalf("frame: %v", top)
	}
}

func TestConcreteTypeNames(t *testing.T) {
	srv := setup(t, faultline.Options{})
	_, err := os.Open("/definitely/not/here")
	faultline.CaptureException(err)
	chain := chainOf(only(t, flush(t, srv)))
	if chain[0]["type"] != "*fs.PathError" || chain[1]["type"] != "syscall.Errno" {
		t.Fatalf("types: %v %v", chain[0]["type"], chain[1]["type"])
	}
}

func TestUnwrapChain(t *testing.T) {
	srv := setup(t, faultline.Options{})
	base := errors.New("connection refused")
	err := fmt.Errorf("load user: %w", fmt.Errorf("query: %w", base))
	faultline.CaptureException(err)
	chain := chainOf(only(t, flush(t, srv)))
	if len(chain) != 3 {
		t.Fatalf("chain length %d", len(chain))
	}
	if chain[0]["type"] != "*fmt.wrapError" || chain[0]["value"] != "load user: query: connection refused" || chain[2]["value"] != "connection refused" {
		t.Fatalf("chain: %v", chain)
	}
	if len(framesOf(chain[0])) == 0 || len(framesOf(chain[1])) != 0 {
		t.Fatal("only the root gets the call-site stack")
	}
}

func TestJoinedErrorsAndDepthLimit(t *testing.T) {
	srv := setup(t, faultline.Options{})
	faultline.CaptureException(errors.Join(errors.New("a"), errors.New("b")))
	deep := errors.New("0")
	for i := 1; i < 10; i++ {
		deep = fmt.Errorf("%d: %w", i, deep)
	}
	faultline.CaptureException(deep)
	events := flush(t, srv)
	if len(events) != 2 {
		t.Fatalf("events %d", len(events))
	}
	joined := chainOf(events[0])
	if len(joined) != 3 || joined[0]["type"] != "*errors.joinError" || joined[1]["value"] != "a" || joined[2]["value"] != "b" {
		t.Fatalf("joined: %v", joined)
	}
	if n := len(chainOf(events[1])); n != 5 {
		t.Fatalf("chain must be capped at 5 exceptions, got %d", n)
	}
}

type cycleErr struct{ next error }

func (e *cycleErr) Error() string { return "cycle" }
func (e *cycleErr) Unwrap() error { return e.next }

func TestCyclicUnwrapTerminates(t *testing.T) {
	srv := setup(t, faultline.Options{})
	a := &cycleErr{}
	b := &cycleErr{next: a}
	a.next = b
	faultline.CaptureException(a)
	if n := len(chainOf(only(t, flush(t, srv)))); n != 2 {
		t.Fatalf("cycle should stop after 2, got %d", n)
	}
}

//go:noinline
func origin() error { return faultline.WithStack(errors.New("created here")) }

// pkg/errors-style error carrying its own StackTrace() of uintptr-kinded frames.
type pkgFrame uintptr
type pkgStack []pkgFrame
type pkgError struct {
	msg   string
	stack pkgStack
}

func (e *pkgError) Error() string        { return e.msg }
func (e *pkgError) StackTrace() pkgStack { return e.stack }

//go:noinline
func newPkgError() error {
	err := origin().(interface{ Callers() []uintptr })
	st := make(pkgStack, 0)
	for _, pc := range err.Callers() {
		st = append(st, pkgFrame(pc))
	}
	return &pkgError{msg: "pkg style", stack: st}
}

func TestWithStackAndOwnStacks(t *testing.T) {
	srv := setup(t, faultline.Options{})
	if faultline.WithStack(nil) != nil {
		t.Fatal("WithStack(nil)")
	}
	err := origin()
	if again := faultline.WithStack(err); again != err {
		t.Fatal("WithStack must not double-wrap")
	}
	if !errors.Is(err, err) || err.Error() != "created here" {
		t.Fatal("message preserved")
	}
	faultline.CaptureException(fmt.Errorf("wrapped: %w", err))
	faultline.CaptureException(newPkgError())
	events := flush(t, srv)
	chain := chainOf(events[0])
	if len(chain) != 2 {
		t.Fatalf("WithStack wrapper should be folded: %v", chain)
	}
	if chain[1]["type"] != "*errors.errorString" || newest(chain[1])["function"] != "faultline-go_test.origin" {
		t.Fatalf("cause should carry the creation stack: %v", newest(chain[1]))
	}
	if newest(chain[0])["function"] != "faultline-go_test.TestWithStackAndOwnStacks" {
		t.Fatalf("root keeps the capture site: %v", newest(chain[0]))
	}
	pkg := exceptionOf(events[1])
	if pkg["type"] != "*faultline_test.pkgError" || newest(pkg)["function"] != "faultline-go_test.origin" {
		t.Fatalf("pkg/errors-style stack: %v %v", pkg["type"], newest(pkg))
	}
}

func TestCaptureMessage(t *testing.T) {
	srv := setup(t, faultline.Options{})
	faultline.CaptureMessage("payment webhook retried", "")
	faultline.CaptureMessage("disk almost full", faultline.LevelWarning)
	events := flush(t, srv)
	if len(events) != 2 || events[0]["level"] != "info" || events[1]["level"] != "warning" || events[0]["exception"] != nil {
		t.Fatalf("messages: %v", events)
	}
}

func TestFilteringOptions(t *testing.T) {
	srv := setup(t, faultline.Options{
		IgnoreErrors: []string{"context canceled", `^ignored-\d+$`},
		BeforeSend: func(e *faultline.Event) *faultline.Event {
			if e.Message == "drop me" {
				return nil
			}
			if e.Message == "panic me" {
				panic("hook bug")
			}
			e.Tags = map[string]string{"hooked": "yes"}
			return e
		},
	})
	faultline.CaptureException(fmt.Errorf("read: %w", errors.New("context canceled")))
	faultline.CaptureMessage("ignored-42", "")
	faultline.CaptureMessage("drop me", "")
	faultline.CaptureMessage("panic me", "")
	faultline.CaptureMessage("kept", "")
	ev := only(t, flush(t, srv))
	if ev["message"] != "kept" || tagsOf(ev)["hooked"] != "yes" {
		t.Fatalf("event: %v", ev)
	}
}

func TestSampleRate(t *testing.T) {
	srv := setup(t, faultline.Options{SampleRate: 1e-12})
	for i := 0; i < 20; i++ {
		faultline.CaptureMessage(fmt.Sprint("m", i), "")
	}
	if n := len(flush(t, srv)); n != 0 {
		t.Fatalf("sampled %d", n)
	}
}

func TestDedupeConsecutive(t *testing.T) {
	srv := setup(t, faultline.Options{})
	err := errors.New("same")
	ids := 0
	for i := 0; i < 3; i++ {
		if faultline.CaptureException(err) != nil {
			ids++
		}
	}
	faultline.CaptureMessage("other", "")
	if ids != 1 || len(flush(t, srv)) != 2 {
		t.Fatalf("expected the duplicate to be dropped (ids=%d)", ids)
	}
}

func TestMaxBreadcrumbsAndScrubbing(t *testing.T) {
	srv := setup(t, faultline.Options{MaxBreadcrumbs: 3})
	for i := 0; i < 10; i++ {
		faultline.AddBreadcrumb(faultline.Breadcrumb{Message: fmt.Sprint(i), Data: map[string]any{"password": "x"}})
	}
	faultline.SetExtra("card", "4111 1111 1111 1111")
	faultline.SetTag("api_key", "secret")
	faultline.CaptureMessage("m", "")
	ev := only(t, flush(t, srv))
	crumbs := ev["breadcrumbs"].([]any)
	if len(crumbs) != 3 || crumbs[0].(map[string]any)["message"] != "7" {
		t.Fatalf("crumbs: %v", crumbs)
	}
	if crumbs[0].(map[string]any)["data"].(map[string]any)["password"] != "[Filtered]" {
		t.Fatal("breadcrumb data scrubbed")
	}
	if ev["extra"].(map[string]any)["card"] != "[Filtered]" || tagsOf(ev)["api_key"] != "[Filtered]" {
		t.Fatalf("scrubbing: %v", ev)
	}
}

func TestInitValidation(t *testing.T) {
	faultline.ResetForTest()
	defer faultline.ResetForTest()
	err := faultline.Init(faultline.Options{})
	if !errors.Is(err, faultline.ErrInvalidOptions) || faultline.IsEnabled() {
		t.Fatalf("missing key: %v", err)
	}
	if err := faultline.Init(faultline.Options{APIKey: "k", Endpoint: "not a url"}); err == nil || faultline.IsEnabled() {
		t.Fatal("bad endpoint must disable")
	}
	if err := faultline.Init(faultline.Options{Enabled: faultline.Bool(false)}); err != nil || faultline.IsEnabled() {
		t.Fatalf("disabled: %v", err)
	}
	if faultline.CaptureMessage("x", "") != nil {
		t.Fatal("disabled SDK returns nil ids")
	}
	err = faultline.Init(faultline.Options{APIKey: "k", Endpoint: "http://127.0.0.1:1", SampleRate: 7})
	if !errors.Is(err, faultline.ErrInvalidOptions) || !faultline.IsEnabled() {
		t.Fatalf("invalid optional values warn but keep the SDK on: %v", err)
	}
	if err := faultline.Init(faultline.Options{APIKey: "k", Endpoint: "http://127.0.0.1:1"}); err != nil {
		t.Fatalf("re-init: %v", err)
	}
}
