package application

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/getfaultline/faultline-go/internal/domain"
)

type scopeKey struct{}

// Hub holds the current client, the global root scope and a stack of WithScope forks.
// Context-carried scopes (NewContext) live in context.Context values instead, so concurrent
// requests never share them.
type Hub struct {
	client atomic.Pointer[Client]
	root   *domain.Scope

	mu    sync.Mutex
	stack []*domain.Scope
}

// NewHub returns a hub with an empty root scope and no client.
func NewHub() *Hub { return &Hub{root: domain.NewScope()} }

// Client returns the bound client (nil when the SDK is not initialized).
func (h *Hub) Client() *Client { return h.client.Load() }

// BindClient replaces the client and returns the previous one.
func (h *Hub) BindClient(c *Client) *Client { return h.client.Swap(c) }

// Logger returns the client's logger, or a no-op logger.
func (h *Hub) Logger() Logger {
	if c := h.Client(); c != nil {
		return c.Logger()
	}
	return NopLogger{}
}

// RootScope returns the global scope.
func (h *Hub) RootScope() *domain.Scope { return h.root }

// CurrentScope returns the innermost WithScope fork, or the root scope.
func (h *Hub) CurrentScope() *domain.Scope {
	h.mu.Lock()
	defer h.mu.Unlock()
	if n := len(h.stack); n > 0 {
		return h.stack[n-1]
	}
	return h.root
}

// PushScope forks the current scope and makes the fork current.
func (h *Hub) PushScope() *domain.Scope {
	s := h.CurrentScope().Clone()
	h.mu.Lock()
	h.stack = append(h.stack, s)
	h.mu.Unlock()
	return s
}

// PopScope removes s from the stack (wherever it is, so interleaved pops stay correct).
func (h *Hub) PopScope(s *domain.Scope) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for i := len(h.stack) - 1; i >= 0; i-- {
		if h.stack[i] == s {
			h.stack = append(h.stack[:i], h.stack[i+1:]...)
			return
		}
	}
}

// NewContext returns a child of ctx carrying a fork of the scope already in ctx (or of the
// current scope when ctx has none).
func (h *Hub) NewContext(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, scopeKey{}, h.ScopeFor(ctx).Clone())
}

// ScopeFromContext returns the scope stored in ctx, or nil.
func ScopeFromContext(ctx context.Context) *domain.Scope {
	if ctx == nil {
		return nil
	}
	s, _ := ctx.Value(scopeKey{}).(*domain.Scope)
	return s
}

// ScopeFor returns the scope in ctx, falling back to the current scope.
func (h *Hub) ScopeFor(ctx context.Context) *domain.Scope {
	if s := ScopeFromContext(ctx); s != nil {
		return s
	}
	return h.CurrentScope()
}

// CaptureException captures err with scope (nil means the current scope).
func (h *Hub) CaptureException(err error, hint Hint, scope *domain.Scope) string {
	c := h.Client()
	if c == nil {
		return ""
	}
	if scope == nil {
		scope = h.CurrentScope()
	}
	return c.CaptureException(err, hint, scope)
}

// CapturePanic captures a recovered panic value with scope (nil means the current scope).
func (h *Hub) CapturePanic(v any, hint Hint, scope *domain.Scope) string {
	c := h.Client()
	if c == nil {
		return ""
	}
	if scope == nil {
		scope = h.CurrentScope()
	}
	return c.CapturePanic(v, hint, scope)
}

// CaptureMessage captures a message with scope (nil means the current scope).
func (h *Hub) CaptureMessage(msg string, level domain.Level, scope *domain.Scope) string {
	c := h.Client()
	if c == nil {
		return ""
	}
	if scope == nil {
		scope = h.CurrentScope()
	}
	return c.CaptureMessage(msg, level, scope)
}

// AddBreadcrumb validates b and appends it to scope (nil means the current scope),
// honoring the client's MaxBreadcrumbs.
func (h *Hub) AddBreadcrumb(b domain.Breadcrumb, scope *domain.Scope) {
	if scope == nil {
		scope = h.CurrentScope()
	}
	if b.Timestamp.IsZero() {
		b.Timestamp = time.Now()
	}
	if !b.Type.Valid() {
		b.Type = domain.BreadcrumbDefault
	}
	if !b.Level.Valid() {
		b.Level = ""
	}
	if b.Data != nil {
		cp := make(map[string]any, len(b.Data))
		for k, v := range b.Data {
			cp[k] = v
		}
		b.Data = cp
	}
	max := domain.MaxBreadcrumbs
	if c := h.Client(); c != nil {
		max = c.Options().MaxBreadcrumbs
	}
	scope.AddBreadcrumb(b, max)
}

// Flush flushes the bound client (true when there is none).
func (h *Hub) Flush(timeout time.Duration) bool {
	if c := h.Client(); c != nil {
		return c.Flush(timeout)
	}
	return true
}

// Close closes and unbinds the bound client.
func (h *Hub) Close(timeout time.Duration) bool {
	c := h.Client()
	if c == nil {
		return true
	}
	ok := c.Close(timeout)
	h.client.CompareAndSwap(c, nil)
	return ok
}
