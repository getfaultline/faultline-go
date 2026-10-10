package faultline

import "runtime"

// WithStack annotates err with the stack trace of the caller, so the event shows where the
// error was created instead of where it was captured. It returns nil for a nil error and err
// unchanged when it already carries a stack (WithStack, github.com/pkg/errors,
// github.com/go-errors/errors). The result has the same message and unwraps to err, so
// errors.Is and errors.As keep working; in the event it is folded into err, which keeps its
// concrete type name.
func WithStack(err error) error {
	if err == nil {
		return nil
	}
	if hasStack(err) {
		return err
	}
	pcs := make([]uintptr, maxCallers)
	n := runtime.Callers(2, pcs)
	return &stackError{err: err, pcs: pcs[:n]}
}

type stackError struct {
	err error
	pcs []uintptr
}

func (e *stackError) Error() string { return e.err.Error() }

func (e *stackError) Unwrap() error { return e.err }

// Callers returns the program counters captured by WithStack (github.com/go-errors/errors
// shape), which the SDK reads when building the event.
func (e *stackError) Callers() []uintptr { return e.pcs }

func hasStack(err error) (ok bool) {
	defer func() {
		if recover() != nil {
			ok = false
		}
	}()
	switch err.(type) {
	case interface{ Callers() []uintptr }:
		return true
	}
	return len(stackParser.FromError(err)) > 0
}
