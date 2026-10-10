package faultline

import (
	"context"
	"runtime"
	"time"

	"github.com/getfaultline/faultline-go/internal/application"
	"github.com/getfaultline/faultline-go/internal/domain"
)

// maxCallers bounds runtime.Callers; frames are expanded (inlining) and capped at 100 later.
const maxCallers = 128

// callers returns the return PCs of the caller of the function that calls callers, skipping
// skip additional frames.
func callers(skip int) []uintptr {
	pcs := make([]uintptr, maxCallers)
	n := runtime.Callers(skip+3, pcs)
	return pcs[:n]
}

func logInternal(r any) {
	defer func() { _ = recover() }()
	hub.Logger().Errorf("internal SDK error: %v", r)
}

// CaptureException captures err with the current scope and returns its event id, or nil
// when err is nil, the SDK is disabled, or the event was dropped (sampling, IgnoreErrors,
// dedupe, BeforeSend). The stack trace is the error's own (see WithStack) or the call site.
func CaptureException(err error) (id *EventID) {
	defer func() {
		if r := recover(); r != nil {
			logInternal(r)
			id = nil
		}
	}()
	if err == nil {
		return nil
	}
	return toEventID(hub.CaptureException(err, application.Hint{PCs: callers(0)}, nil))
}

// CaptureExceptionContext is CaptureException using the scope carried by ctx (see
// NewContext), falling back to the current scope.
func CaptureExceptionContext(ctx context.Context, err error) (id *EventID) {
	defer func() {
		if r := recover(); r != nil {
			logInternal(r)
			id = nil
		}
	}()
	if err == nil {
		return nil
	}
	return toEventID(hub.CaptureException(err, application.Hint{PCs: callers(0)}, hub.ScopeFor(ctx)))
}

// CaptureMessage captures a plain message. An empty level means LevelInfo.
func CaptureMessage(message string, level Level) (id *EventID) {
	defer func() {
		if r := recover(); r != nil {
			logInternal(r)
			id = nil
		}
	}()
	return toEventID(hub.CaptureMessage(message, level, nil))
}

// CaptureMessageContext is CaptureMessage using the scope carried by ctx.
func CaptureMessageContext(ctx context.Context, message string, level Level) (id *EventID) {
	defer func() {
		if r := recover(); r != nil {
			logInternal(r)
			id = nil
		}
	}()
	return toEventID(hub.CaptureMessage(message, level, hub.ScopeFor(ctx)))
}

// Recover captures a panic at level "fatal". It must be deferred directly:
//
//	defer faultline.Recover()
//
// Unless Options.SwallowPanics is set, it then flushes for up to 2s and re-panics with the
// original value, so the program crashes exactly as it would without the SDK.
func Recover() {
	v := recover()
	if v == nil {
		return
	}
	handlePanic(context.Background(), v)
}

// RecoverWithContext is Recover using the scope carried by ctx. It must be deferred directly:
//
//	defer faultline.RecoverWithContext(ctx)
func RecoverWithContext(ctx context.Context) {
	v := recover()
	if v == nil {
		return
	}
	handlePanic(ctx, v)
}

func handlePanic(ctx context.Context, v any) {
	swallow := false
	func() {
		defer func() {
			if r := recover(); r != nil {
				logInternal(r)
			}
		}()
		hub.CapturePanic(v, application.Hint{PCs: callers(1), Panicking: true, Level: domain.LevelFatal}, hub.ScopeFor(ctx))
		if c := hub.Client(); c != nil {
			swallow = c.Options().SwallowPanics
			if !swallow {
				c.Flush(2 * time.Second)
			}
		}
	}()
	if !swallow {
		panic(v)
	}
}

// CapturePanic captures a value returned by recover() at level "fatal", using the scope
// carried by ctx. Call it from the deferred function that recovered, so the
// stack trace points at the panic. It never re-panics; framework middleware uses it.
func CapturePanic(ctx context.Context, recovered any) (id *EventID) {
	defer func() {
		if r := recover(); r != nil {
			logInternal(r)
			id = nil
		}
	}()
	if recovered == nil {
		return nil
	}
	hint := application.Hint{PCs: callers(0), Panicking: true, Level: domain.LevelFatal}
	return toEventID(hub.CapturePanic(recovered, hint, hub.ScopeFor(ctx)))
}

// SetUser sets the user on the current scope. The zero User clears it.
func SetUser(u User) { currentScope().SetUser(u) }

// SetTag sets a searchable tag on the current scope.
func SetTag(key, value string) { currentScope().SetTag(key, value) }

// SetTags merges tags into the current scope.
func SetTags(tags map[string]string) { currentScope().SetTags(tags) }

// SetContext sets structured context on the current scope. A nil map removes it.
func SetContext(name string, ctx map[string]any) { currentScope().SetContext(name, ctx) }

// SetExtra sets arbitrary extra data on the current scope.
func SetExtra(key string, value any) { currentScope().SetExtra(key, value) }

// AddBreadcrumb records a breadcrumb on the current scope.
func AddBreadcrumb(b Breadcrumb) { currentScope().AddBreadcrumb(b) }

// AddBreadcrumbContext records a breadcrumb on the scope carried by ctx, falling back to
// the current scope.
func AddBreadcrumbContext(ctx context.Context, b Breadcrumb) {
	defer func() {
		if r := recover(); r != nil {
			logInternal(r)
		}
	}()
	hub.AddBreadcrumb(b, hub.ScopeFor(ctx))
}

// WithScope runs f with a fork of the current scope. Changes made to the scope apply only to
// events captured inside f, either through the scope's own methods or through the package
// functions (CaptureException, SetTag, ...). Panics raised by f propagate unchanged.
//
// The fork is pushed onto a process-wide stack, so package functions called concurrently
// from other goroutines while f runs also see it. In concurrent code (HTTP handlers, worker
// pools) prefer the scope's own Capture methods or a context-carried scope (NewContext).
func WithScope(f func(scope *Scope)) {
	if f == nil {
		return
	}
	var s *domain.Scope
	func() {
		defer func() {
			if r := recover(); r != nil {
				logInternal(r)
			}
		}()
		s = hub.PushScope()
	}()
	if s == nil {
		f(nil)
		return
	}
	defer hub.PopScope(s)
	f(&Scope{s: s})
}

// NewContext returns a child of ctx that carries its own scope: a fork of the scope already
// in ctx, or of the current scope. Data set on it (ScopeFromContext(ctx).SetTag, ...) is
// visible only to events captured with that context, so concurrent requests never leak into
// each other.
func NewContext(ctx context.Context) (out context.Context) {
	defer func() {
		if r := recover(); r != nil {
			logInternal(r)
			out = ctx
		}
	}()
	return hub.NewContext(ctx)
}

// ScopeFromContext returns the scope carried by ctx, or nil when there is none. All Scope
// methods are no-ops on a nil *Scope.
func ScopeFromContext(ctx context.Context) *Scope {
	s := application.ScopeFromContext(ctx)
	if s == nil {
		return nil
	}
	return &Scope{s: s}
}

// CurrentScope returns the innermost WithScope fork or the global scope.
func CurrentScope() *Scope { return currentScope() }

func currentScope() (s *Scope) {
	defer func() {
		if r := recover(); r != nil {
			logInternal(r)
			s = nil
		}
	}()
	return &Scope{s: hub.CurrentScope()}
}
