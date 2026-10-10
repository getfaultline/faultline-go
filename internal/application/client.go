package application

import (
	"fmt"
	"reflect"
	"sync"
	"sync/atomic"
	"time"

	"github.com/getfaultline/faultline-go/internal/domain"
)

// DedupeWindow is how long an identical consecutive event is suppressed.
const DedupeWindow = time.Second

// Deps are the adapters a Client needs.
type Deps struct {
	Transport  Transport
	Stack      StackParser
	Source     SourceContextReader // optional
	Clock      Clock
	IDs        IDGenerator
	Random     Random
	Logger     Logger
	Runtime    RuntimeInfo
	SDKVersion string
}

// Hint carries per-capture data from the public API.
type Hint struct {
	// Level overrides the scope level and the default.
	Level domain.Level
	// PCs is the call-site stack from runtime.Callers (newest first). It is used for the root
	// exception when the error carries no stack of its own.
	PCs []uintptr
	// Panicking marks PCs captured inside a deferred recover; frames above runtime.gopanic
	// are removed.
	Panicking bool
}

// Client builds events, applies scope data, sampling, ignore rules, dedupe, source context,
// scrubbing and BeforeSend, then hands events to the transport. No method panics.
type Client struct {
	opts   domain.Options
	deps   Deps
	closed atomic.Bool

	mu      sync.Mutex
	lastKey string
	lastAt  time.Time
}

// NewClient wires a client. Missing optional deps get safe defaults.
func NewClient(opts domain.Options, deps Deps) *Client {
	if deps.Logger == nil {
		deps.Logger = NopLogger{}
	}
	return &Client{opts: opts, deps: deps}
}

// Options returns the resolved options.
func (c *Client) Options() domain.Options { return c.opts }

// Logger returns the client's logger.
func (c *Client) Logger() Logger { return c.deps.Logger }

// Enabled reports whether the client still accepts events.
func (c *Client) Enabled() bool { return c.opts.Enabled && !c.closed.Load() }

// CaptureException captures err. It returns the event id, or "" when the event was dropped.
func (c *Client) CaptureException(err error, hint Hint, scope *domain.Scope) (id string) {
	defer c.guard("CaptureException", &id)
	if err == nil || !c.Enabled() {
		return ""
	}
	ex := c.buildException(err, hint)
	e := c.baseEvent(hint.Level, domain.LevelError, scope)
	e.Exception = ex
	e.Message = ex.Value
	return c.process(e, scope)
}

// CapturePanic captures a recovered panic value. Errors keep their concrete type; other
// values become an exception of type "panic" whose value is fmt.Sprint(v).
func (c *Client) CapturePanic(v any, hint Hint, scope *domain.Scope) (id string) {
	defer c.guard("CapturePanic", &id)
	if v == nil || !c.Enabled() {
		return ""
	}
	if hint.Level == "" {
		hint.Level = domain.LevelFatal
	}
	if err, ok := v.(error); ok {
		return c.CaptureException(err, hint, scope)
	}
	ex := &domain.Exception{
		Type:       "panic",
		Value:      domain.Truncate(safeSprint(v), domain.MaxMessageLength),
		Stacktrace: domain.Stacktrace{Frames: c.frames(hint.PCs, hint.Panicking)},
	}
	e := c.baseEvent(hint.Level, domain.LevelFatal, scope)
	e.Exception = ex
	e.Message = ex.Value
	return c.process(e, scope)
}

// CaptureMessage captures a plain message ("info" unless a level is given).
func (c *Client) CaptureMessage(message string, level domain.Level, scope *domain.Scope) (id string) {
	defer c.guard("CaptureMessage", &id)
	if !c.Enabled() {
		return ""
	}
	e := c.baseEvent(level, domain.LevelInfo, scope)
	e.Message = domain.Truncate(message, domain.MaxMessageLength)
	return c.process(e, scope)
}

// Flush waits for queued events to be delivered.
func (c *Client) Flush(timeout time.Duration) (ok bool) {
	defer c.guardBool("Flush", &ok)
	return c.deps.Transport.Flush(timeout)
}

// Close flushes and permanently disables the client.
func (c *Client) Close(timeout time.Duration) (ok bool) {
	defer c.guardBool("Close", &ok)
	ok = c.deps.Transport.Flush(timeout)
	c.closed.Store(true)
	c.deps.Transport.Close(0)
	return ok
}

// ---------------------------------------------------------------------------------------

func (c *Client) guard(op string, id *string) {
	if r := recover(); r != nil {
		c.deps.Logger.Errorf("%s failed: %v", op, r)
		*id = ""
	}
}

func (c *Client) guardBool(op string, ok *bool) {
	if r := recover(); r != nil {
		c.deps.Logger.Errorf("%s failed: %v", op, r)
		*ok = false
	}
}

func (c *Client) now() time.Time {
	if c.deps.Clock != nil {
		return c.deps.Clock.Now()
	}
	return time.Now()
}

func (c *Client) baseEvent(level, fallback domain.Level, scope *domain.Scope) *domain.Event {
	if !level.Valid() {
		level = ""
		if scope != nil {
			if l := scope.Data().Level; l.Valid() {
				level = l
			}
		}
		if level == "" {
			level = fallback
		}
	}
	e := &domain.Event{
		EventID:     c.deps.IDs.NewID(),
		Timestamp:   c.now(),
		Level:       level,
		Platform:    domain.Platform,
		Environment: c.opts.Environment,
		Release:     c.opts.Release,
		ServerName:  c.opts.ServerName,
		SDK:         domain.SDKInfo{Name: domain.SDKName, Version: c.deps.SDKVersion},
	}
	if e.ServerName == "" && c.deps.Runtime != nil {
		e.ServerName = c.deps.Runtime.ServerName()
	}
	return e
}

func (c *Client) process(e *domain.Event, scope *domain.Scope) string {
	log := c.deps.Logger
	if c.opts.SampleRate < 1 && c.deps.Random != nil && c.deps.Random.Float64() >= c.opts.SampleRate {
		log.Debugf("event %s dropped by SampleRate", e.EventID)
		return ""
	}

	c.applyScope(e, scope)

	if domain.MatchesIgnoreErrors(e, c.opts.IgnoreErrors) {
		log.Debugf("event %s dropped by IgnoreErrors", e.EventID)
		return ""
	}

	key := domain.DedupeKey(e)
	now := c.now()
	c.mu.Lock()
	if key == c.lastKey && now.Sub(c.lastAt) < DedupeWindow && now.Sub(c.lastAt) >= 0 {
		c.mu.Unlock()
		log.Debugf("event %s dropped as duplicate", e.EventID)
		return ""
	}
	c.lastKey, c.lastAt = key, now
	c.mu.Unlock()

	if e.Exception != nil && c.deps.Source != nil {
		for ex := e.Exception; ex != nil; ex = ex.Cause {
			ex.Stacktrace.Frames = c.addSource(ex.Stacktrace.Frames)
		}
	}

	prepared := domain.SanitizeEvent(e, c.deps.SDKVersion, now)
	if prepared == nil {
		return ""
	}
	prepared = domain.ScrubEvent(prepared, c.opts.SendDefaultPII)

	if c.opts.BeforeSend != nil {
		result, ok := c.runBeforeSend(prepared)
		if !ok {
			return ""
		}
		if result == nil {
			log.Debugf("event %s dropped by BeforeSend", prepared.EventID)
			return ""
		}
		after := domain.SanitizeEvent(result, c.deps.SDKVersion, now)
		if after == nil {
			log.Warnf("BeforeSend returned an invalid event; dropped")
			return ""
		}
		prepared = after
	}

	if c.closed.Load() {
		return ""
	}
	c.deps.Transport.Send(prepared)
	return prepared.EventID
}

func (c *Client) runBeforeSend(e *domain.Event) (out *domain.Event, ok bool) {
	defer func() {
		if r := recover(); r != nil {
			c.deps.Logger.Warnf("BeforeSend panicked (%v); event %s dropped", r, e.EventID)
			out, ok = nil, false
		}
	}()
	return c.opts.BeforeSend(e), true
}

func (c *Client) addSource(frames []domain.StackFrame) (out []domain.StackFrame) {
	defer func() {
		if r := recover(); r != nil {
			c.deps.Logger.Debugf("source context failed: %v", r)
			out = frames
		}
	}()
	return c.deps.Source.AddSourceContext(frames)
}

func (c *Client) applyScope(e *domain.Event, scope *domain.Scope) {
	var d domain.ScopeData
	if scope != nil {
		d = scope.Data()
	}
	if len(d.Tags) > 0 {
		e.Tags = d.Tags
	}
	if !d.User.IsZero() {
		u := d.User
		e.User = &u
	}
	if d.Request != nil {
		e.Request = d.Request
	}
	e.Contexts = map[string]any{}
	if c.deps.Runtime != nil {
		for k, v := range c.deps.Runtime.Contexts() {
			e.Contexts[k] = v
		}
	}
	for k, v := range d.Contexts {
		e.Contexts[k] = v
	}
	if len(d.Extra) > 0 {
		e.Extra = d.Extra
	}
	if max := c.opts.MaxBreadcrumbs; max > 0 && len(d.Breadcrumbs) > 0 {
		crumbs := d.Breadcrumbs
		if len(crumbs) > max {
			crumbs = crumbs[len(crumbs)-max:]
		}
		e.Breadcrumbs = crumbs
	}
	if len(d.Fingerprint) > 0 {
		e.Fingerprint = d.Fingerprint
	}
}

// ---------------------------------------------------------------------------------------
// Exception chain

type link struct {
	err error
	pcs []uintptr
}

// buildException walks the unwrap tree depth-first (errors.Unwrap and Unwrap() []error),
// keeping at most MaxExceptionDepth errors linked as a cause chain. A wrapper that only adds
// a stack (same message as its single child, like pkg/errors withStack or WithStack) is
// folded into its child, which inherits the stack when it has none.
func (c *Client) buildException(root error, hint Hint) *domain.Exception {
	var links []link
	seen := map[error]bool{}
	var inherited []uintptr
	var visit func(e error, depth int)
	visit = func(e error, depth int) {
		if e == nil || len(links) >= domain.MaxExceptionDepth || depth > 32 {
			return
		}
		if isComparable(e) {
			if seen[e] {
				return
			}
			seen[e] = true
		}
		pcs := c.errorPCs(e)
		children := unwrapAll(e)
		if len(pcs) > 0 && len(children) == 1 && children[0] != nil &&
			domain.SafeErrorString(children[0]) == domain.SafeErrorString(e) {
			if inherited == nil {
				inherited = pcs
			}
			visit(children[0], depth+1)
			return
		}
		if len(pcs) == 0 && inherited != nil {
			pcs = inherited
		}
		inherited = nil
		links = append(links, link{err: e, pcs: pcs})
		for _, ch := range children {
			visit(ch, depth+1)
		}
	}
	visit(root, 0)
	if len(links) == 0 {
		links = append(links, link{err: root})
	}

	var head, tail *domain.Exception
	for i, l := range links {
		var frames []domain.StackFrame
		switch {
		case len(l.pcs) > 0:
			frames = c.frames(l.pcs, false)
		case i == 0:
			frames = c.frames(hint.PCs, hint.Panicking)
		}
		ex := &domain.Exception{
			Type:       typeName(l.err),
			Value:      domain.Truncate(domain.SafeErrorString(l.err), domain.MaxMessageLength),
			Stacktrace: domain.Stacktrace{Frames: frames},
		}
		if ex.Stacktrace.Frames == nil {
			ex.Stacktrace.Frames = []domain.StackFrame{}
		}
		if head == nil {
			head = ex
		} else {
			tail.Cause = ex
		}
		tail = ex
	}
	return head
}

func (c *Client) errorPCs(err error) (pcs []uintptr) {
	defer func() {
		if r := recover(); r != nil {
			pcs = nil
		}
	}()
	if c.deps.Stack == nil {
		return nil
	}
	return c.deps.Stack.FromError(err)
}

func (c *Client) frames(pcs []uintptr, panicking bool) (out []domain.StackFrame) {
	defer func() {
		if r := recover(); r != nil {
			c.deps.Logger.Debugf("stack parsing failed: %v", r)
			out = []domain.StackFrame{}
		}
	}()
	if len(pcs) == 0 || c.deps.Stack == nil {
		return []domain.StackFrame{}
	}
	return c.deps.Stack.Frames(pcs, panicking)
}

func unwrapAll(err error) (out []error) {
	defer func() {
		if r := recover(); r != nil {
			out = nil
		}
	}()
	switch u := err.(type) {
	case interface{ Unwrap() []error }:
		return u.Unwrap()
	case interface{ Unwrap() error }:
		if next := u.Unwrap(); next != nil {
			return []error{next}
		}
	}
	return nil
}

func isComparable(err error) bool {
	t := reflect.TypeOf(err)
	return t != nil && t.Comparable()
}

func typeName(err error) string {
	return fmt.Sprintf("%T", err)
}

func safeSprint(v any) (s string) {
	defer func() {
		if r := recover(); r != nil {
			s = fmt.Sprintf("[%T: String() panicked]", v)
		}
	}()
	return fmt.Sprint(v)
}
