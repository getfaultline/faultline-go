// Package application holds the use cases of the SDK: the Client (builds, filters and
// enriches events) and the Hub (current client plus scopes). It depends only on the domain
// layer and on the ports declared here; infrastructure adapters implement the ports.
package application

import (
	"time"

	"github.com/getfaultline/faultline-go/internal/domain"
)

// Transport delivers events to the ingest API. Implementations must never panic or block.
type Transport interface {
	// Send enqueues an event for background delivery.
	Send(e *domain.Event)
	// Flush waits until everything queued so far was delivered (or dropped). False on timeout.
	Flush(timeout time.Duration) bool
	// Close flushes, then stops accepting events and releases the background goroutine.
	Close(timeout time.Duration) bool
}

// StackParser turns program counters into contract frames (oldest to newest).
type StackParser interface {
	// FromError returns the program counters an error carries (pkg/errors-style
	// StackTrace(), Callers() []uintptr), or nil when it carries none.
	FromError(err error) []uintptr
	// Frames converts return PCs from runtime.Callers (newest first) into frames ordered
	// oldest to newest, keeping the newest MaxFrames. With panicking=true, frames above
	// runtime.gopanic (the deferred recovery machinery) are removed.
	Frames(pcs []uintptr, panicking bool) []domain.StackFrame
}

// SourceContextReader adds pre/post/context lines to in-app frames. It returns the input
// unchanged on failure.
type SourceContextReader interface {
	AddSourceContext(frames []domain.StackFrame) []domain.StackFrame
}

// Clock returns the current time.
type Clock interface{ Now() time.Time }

// IDGenerator returns lowercase uuid v4 strings with dashes.
type IDGenerator interface{ NewID() string }

// Random returns a uniform float in [0, 1).
type Random interface{ Float64() float64 }

// Logger logs SDK internals. Implementations log only in debug mode.
type Logger interface {
	Debugf(format string, args ...any)
	Warnf(format string, args ...any)
	Errorf(format string, args ...any)
}

// RuntimeInfo describes the host process.
type RuntimeInfo interface {
	// Contexts returns contexts.runtime and contexts.os.
	Contexts() map[string]any
	ServerName() string
}

// NopLogger discards everything.
type NopLogger struct{}

// Debugf implements Logger.
func (NopLogger) Debugf(string, ...any) {}

// Warnf implements Logger.
func (NopLogger) Warnf(string, ...any) {}

// Errorf implements Logger.
func (NopLogger) Errorf(string, ...any) {}
