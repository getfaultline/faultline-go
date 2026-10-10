package application

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/getfaultline/faultline-go/internal/domain"
)

type fakeTransport struct {
	mu     sync.Mutex
	events []*domain.Event
}

func (f *fakeTransport) Send(e *domain.Event) {
	f.mu.Lock()
	f.events = append(f.events, e)
	f.mu.Unlock()
}
func (f *fakeTransport) Flush(time.Duration) bool { return true }
func (f *fakeTransport) Close(time.Duration) bool { return true }

type fakeClock struct{ t time.Time }

func (c *fakeClock) Now() time.Time { return c.t }

type seqIDs struct{ n int }

func (s *seqIDs) NewID() string {
	s.n++
	return fmt.Sprintf("00000000-0000-4000-8000-%012d", s.n)
}

func newTestClient(opts domain.Options) (*Client, *fakeTransport, *fakeClock) {
	opts.Enabled = true
	if opts.SampleRate == 0 {
		opts.SampleRate = 1
	}
	tr := &fakeTransport{}
	clock := &fakeClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	return NewClient(opts, Deps{Transport: tr, Clock: clock, IDs: &seqIDs{}, SDKVersion: "t"}), tr, clock
}

func TestDedupeWindow(t *testing.T) {
	c, tr, clock := newTestClient(domain.Options{})
	scope := domain.NewScope()
	err := errors.New("same")
	c.CaptureException(err, Hint{}, scope)
	clock.t = clock.t.Add(500 * time.Millisecond)
	if id := c.CaptureException(err, Hint{}, scope); id != "" {
		t.Fatal("duplicate within 1s must be dropped")
	}
	clock.t = clock.t.Add(1100 * time.Millisecond)
	if id := c.CaptureException(err, Hint{}, scope); id == "" {
		t.Fatal("after the window the error is reported again")
	}
	c.CaptureMessage("m", "", scope)
	c.CaptureMessage("m", domain.LevelWarning, scope) // different level: different key
	if len(tr.events) != 4 {
		t.Fatalf("events %d", len(tr.events))
	}
}

func TestLevelPrecedenceAndClose(t *testing.T) {
	c, tr, _ := newTestClient(domain.Options{})
	scope := domain.NewScope()
	scope.SetLevel(domain.LevelWarning)
	c.CaptureException(errors.New("a"), Hint{}, scope)
	c.CaptureException(errors.New("b"), Hint{Level: domain.LevelFatal}, scope)
	c.CapturePanic("p", Hint{}, domain.NewScope())
	if tr.events[0].Level != domain.LevelWarning || tr.events[1].Level != domain.LevelFatal || tr.events[2].Level != domain.LevelFatal {
		t.Fatalf("levels: %s %s %s", tr.events[0].Level, tr.events[1].Level, tr.events[2].Level)
	}
	if tr.events[2].Exception.Type != "panic" || tr.events[2].Exception.Value != "p" {
		t.Fatalf("panic: %+v", tr.events[2].Exception)
	}
	c.Close(time.Second)
	if c.Enabled() || c.CaptureMessage("late", "", scope) != "" {
		t.Fatal("closed client must drop")
	}
}

func TestHubStackAndContext(t *testing.T) {
	h := NewHub()
	h.RootScope().SetTag("root", "1")
	a := h.PushScope()
	b := h.PushScope()
	if h.CurrentScope() != b {
		t.Fatal("top of stack")
	}
	h.PopScope(a) // out of order
	if h.CurrentScope() != b {
		t.Fatal("pop out of order")
	}
	h.PopScope(b)
	if h.CurrentScope() != h.RootScope() {
		t.Fatal("empty stack falls back to root")
	}
	ctx := h.NewContext(context.Background())
	s := ScopeFromContext(ctx)
	if s == nil || s.Data().Tags["root"] != "1" || h.ScopeFor(ctx) != s {
		t.Fatal("context scope forks the current scope")
	}
	if h.CaptureMessage("no client", "", nil) != "" {
		t.Fatal("no client, no event")
	}
}
