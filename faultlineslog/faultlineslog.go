// Package faultlineslog turns log/slog records into Faultline breadcrumbs and, optionally,
// events.
//
//	logger := slog.New(faultlineslog.NewHandler(slog.NewJSONHandler(os.Stderr, nil), &faultlineslog.Options{
//		EventLevel: slog.LevelError, // records >= Error also become events
//	}))
//
// Breadcrumbs go to the scope carried by the record's context (faultline.NewContext), or to
// the current scope. Records at EventLevel or above become exception events when they carry
// an error attribute, and message events otherwise.
package faultlineslog

import (
	"context"
	"log/slog"

	faultline "github.com/getfaultline/faultline-go"
)

// Options configures the handler.
type Options struct {
	// BreadcrumbLevel is the minimum level recorded as a breadcrumb (default slog.LevelInfo).
	BreadcrumbLevel slog.Leveler
	// EventLevel is the minimum level captured as an event. Nil (default) disables events.
	EventLevel slog.Leveler
	// Category of the breadcrumbs (default "slog").
	Category string
}

// Handler is a slog.Handler that records breadcrumbs and forwards to an optional next handler.
type Handler struct {
	next   slog.Handler
	opts   Options
	attrs  []groupedAttr
	groups []string
}

type groupedAttr struct {
	groups []string // groups open when WithAttrs was called (never mutated)
	attr   slog.Attr
}

// NewHandler returns a handler that forwards records to next (which may be nil to only feed
// Faultline).
func NewHandler(next slog.Handler, opts *Options) *Handler {
	h := &Handler{next: next}
	if opts != nil {
		h.opts = *opts
	}
	if h.opts.BreadcrumbLevel == nil {
		h.opts.BreadcrumbLevel = slog.LevelInfo
	}
	if h.opts.Category == "" {
		h.opts.Category = "slog"
	}
	return h
}

// Enabled implements slog.Handler.
func (h *Handler) Enabled(ctx context.Context, level slog.Level) bool {
	if level >= h.opts.BreadcrumbLevel.Level() {
		return true
	}
	if h.opts.EventLevel != nil && level >= h.opts.EventLevel.Level() {
		return true
	}
	return h.next != nil && h.next.Enabled(ctx, level)
}

// Handle implements slog.Handler. Faultline never makes it fail; the next handler's error is
// returned unchanged.
func (h *Handler) Handle(ctx context.Context, r slog.Record) error {
	if ctx == nil {
		ctx = context.Background()
	}
	h.record(ctx, r)
	if h.next != nil && h.next.Enabled(ctx, r.Level) {
		return h.next.Handle(ctx, r)
	}
	return nil
}

func (h *Handler) record(ctx context.Context, r slog.Record) {
	defer func() { _ = recover() }()
	if r.Level < h.opts.BreadcrumbLevel.Level() && (h.opts.EventLevel == nil || r.Level < h.opts.EventLevel.Level()) {
		return
	}
	data := map[string]any{}
	var errAttr error
	var walk func(prefix []string, a slog.Attr)
	walk = func(prefix []string, a slog.Attr) {
		a.Value = a.Value.Resolve()
		if a.Equal(slog.Attr{}) {
			return
		}
		if a.Value.Kind() == slog.KindGroup {
			p := prefix
			if a.Key != "" {
				p = append(append([]string(nil), prefix...), a.Key)
			}
			for _, ga := range a.Value.Group() {
				walk(p, ga)
			}
			return
		}
		key := a.Key
		for i := len(prefix) - 1; i >= 0; i-- {
			key = prefix[i] + "." + key
		}
		v := a.Value.Any()
		if err, ok := v.(error); ok {
			if errAttr == nil {
				errAttr = err
			}
			data[key] = safeError(err)
			return
		}
		data[key] = v
	}
	for _, ga := range h.attrs {
		walk(ga.groups, ga.attr)
	}
	r.Attrs(func(a slog.Attr) bool {
		walk(h.groups, a)
		return true
	})

	if r.Level >= h.opts.BreadcrumbLevel.Level() {
		b := faultline.Breadcrumb{
			Type:     faultline.BreadcrumbLog,
			Category: h.opts.Category,
			Message:  r.Message,
			Level:    toLevel(r.Level),
			Data:     data,
		}
		if !r.Time.IsZero() {
			b.Timestamp = r.Time
		}
		if len(data) == 0 {
			b.Data = nil
		}
		faultline.AddBreadcrumbContext(ctx, b)
	}

	if h.opts.EventLevel != nil && r.Level >= h.opts.EventLevel.Level() {
		if errAttr != nil {
			faultline.CaptureExceptionContext(ctx, errAttr)
		} else {
			faultline.CaptureMessageContext(ctx, r.Message, toLevel(r.Level))
		}
	}
}

// WithAttrs implements slog.Handler.
func (h *Handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	c := h.clone()
	for _, a := range attrs {
		c.attrs = append(c.attrs, groupedAttr{groups: h.groups, attr: a})
	}
	if h.next != nil {
		c.next = h.next.WithAttrs(attrs)
	}
	return c
}

// WithGroup implements slog.Handler.
func (h *Handler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	c := h.clone()
	c.groups = append(append([]string(nil), h.groups...), name)
	if h.next != nil {
		c.next = h.next.WithGroup(name)
	}
	return c
}

func (h *Handler) clone() *Handler {
	return &Handler{
		next:   h.next,
		opts:   h.opts,
		attrs:  append([]groupedAttr(nil), h.attrs...),
		groups: append([]string(nil), h.groups...),
	}
}

func toLevel(l slog.Level) faultline.Level {
	switch {
	case l >= slog.LevelError:
		return faultline.LevelError
	case l >= slog.LevelWarn:
		return faultline.LevelWarning
	case l >= slog.LevelInfo:
		return faultline.LevelInfo
	default:
		return faultline.LevelDebug
	}
}

func safeError(err error) (s string) {
	defer func() {
		if recover() != nil {
			s = "[Error() panicked]"
		}
	}()
	return err.Error()
}
