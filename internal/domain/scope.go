package domain

import "sync"

// Scope is a concurrency-safe bag of contextual data applied to every event captured while it
// is active. It is pure data: the application layer decides which scope is current.
type Scope struct {
	mu          sync.RWMutex
	tags        map[string]string
	user        User
	contexts    map[string]map[string]any
	extra       map[string]any
	breadcrumbs []Breadcrumb
	level       Level
	fingerprint []string
	request     *Request
}

// NewScope returns an empty scope.
func NewScope() *Scope {
	return &Scope{
		tags:     map[string]string{},
		contexts: map[string]map[string]any{},
		extra:    map[string]any{},
	}
}

// Clone returns an independent copy (maps and slices are copied, values are shared).
func (s *Scope) Clone() *Scope {
	s.mu.RLock()
	defer s.mu.RUnlock()
	c := NewScope()
	for k, v := range s.tags {
		c.tags[k] = v
	}
	c.user = s.user
	for k, v := range s.contexts {
		c.contexts[k] = v
	}
	for k, v := range s.extra {
		c.extra[k] = v
	}
	c.breadcrumbs = append([]Breadcrumb(nil), s.breadcrumbs...)
	c.level = s.level
	c.fingerprint = append([]string(nil), s.fingerprint...)
	if s.request != nil {
		r := copyRequest(*s.request)
		c.request = &r
	}
	return c
}

// SetTag sets one tag.
func (s *Scope) SetTag(key, value string) {
	s.mu.Lock()
	s.tags[key] = value
	s.mu.Unlock()
}

// SetTags merges tags.
func (s *Scope) SetTags(tags map[string]string) {
	s.mu.Lock()
	for k, v := range tags {
		s.tags[k] = v
	}
	s.mu.Unlock()
}

// SetUser replaces the user. The zero User clears it.
func (s *Scope) SetUser(u User) {
	s.mu.Lock()
	s.user = u
	s.mu.Unlock()
}

// User returns the current user.
func (s *Scope) User() User {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.user
}

// SetContext sets a named context. A nil map removes it.
func (s *Scope) SetContext(name string, ctx map[string]any) {
	s.mu.Lock()
	if ctx == nil {
		delete(s.contexts, name)
	} else {
		cp := make(map[string]any, len(ctx))
		for k, v := range ctx {
			cp[k] = v
		}
		s.contexts[name] = cp
	}
	s.mu.Unlock()
}

// SetExtra sets one extra value.
func (s *Scope) SetExtra(key string, value any) {
	s.mu.Lock()
	s.extra[key] = value
	s.mu.Unlock()
}

// SetExtras merges extra values.
func (s *Scope) SetExtras(extras map[string]any) {
	s.mu.Lock()
	for k, v := range extras {
		s.extra[k] = v
	}
	s.mu.Unlock()
}

// SetLevel overrides the level of events captured with this scope ("" clears it).
func (s *Scope) SetLevel(l Level) {
	s.mu.Lock()
	s.level = l
	s.mu.Unlock()
}

// SetFingerprint overrides grouping for events captured with this scope (nil clears it).
func (s *Scope) SetFingerprint(fp []string) {
	s.mu.Lock()
	s.fingerprint = append([]string(nil), fp...)
	s.mu.Unlock()
}

// SetRequest attaches request data (nil clears it).
func (s *Scope) SetRequest(r *Request) {
	s.mu.Lock()
	if r == nil {
		s.request = nil
	} else {
		cp := copyRequest(*r)
		s.request = &cp
	}
	s.mu.Unlock()
}

// AddBreadcrumb appends b, keeping at most max (itself capped at MaxBreadcrumbs) entries.
func (s *Scope) AddBreadcrumb(b Breadcrumb, max int) {
	if max > MaxBreadcrumbs {
		max = MaxBreadcrumbs
	}
	if max <= 0 {
		return
	}
	s.mu.Lock()
	s.breadcrumbs = append(s.breadcrumbs, b)
	if n := len(s.breadcrumbs); n > max {
		s.breadcrumbs = append([]Breadcrumb(nil), s.breadcrumbs[n-max:]...)
	}
	s.mu.Unlock()
}

// ClearBreadcrumbs removes all breadcrumbs.
func (s *Scope) ClearBreadcrumbs() {
	s.mu.Lock()
	s.breadcrumbs = nil
	s.mu.Unlock()
}

// Clear resets the scope to empty.
func (s *Scope) Clear() {
	s.mu.Lock()
	s.tags = map[string]string{}
	s.user = User{}
	s.contexts = map[string]map[string]any{}
	s.extra = map[string]any{}
	s.breadcrumbs = nil
	s.level = ""
	s.fingerprint = nil
	s.request = nil
	s.mu.Unlock()
}

// ScopeData is a read-only snapshot of a scope.
type ScopeData struct {
	Tags        map[string]string
	User        User
	Contexts    map[string]map[string]any
	Extra       map[string]any
	Breadcrumbs []Breadcrumb
	Level       Level
	Fingerprint []string
	Request     *Request
}

// Data returns a snapshot that later scope changes do not affect.
func (s *Scope) Data() ScopeData {
	s.mu.RLock()
	defer s.mu.RUnlock()
	d := ScopeData{
		Tags:        make(map[string]string, len(s.tags)),
		User:        s.user,
		Contexts:    make(map[string]map[string]any, len(s.contexts)),
		Extra:       make(map[string]any, len(s.extra)),
		Breadcrumbs: append([]Breadcrumb(nil), s.breadcrumbs...),
		Level:       s.level,
		Fingerprint: append([]string(nil), s.fingerprint...),
	}
	for k, v := range s.tags {
		d.Tags[k] = v
	}
	for k, v := range s.contexts {
		d.Contexts[k] = v
	}
	for k, v := range s.extra {
		d.Extra[k] = v
	}
	if s.request != nil {
		r := copyRequest(*s.request)
		d.Request = &r
	}
	return d
}

func copyRequest(r Request) Request {
	out := Request{Method: r.Method, URL: r.URL}
	if r.Headers != nil {
		out.Headers = make(map[string]string, len(r.Headers))
		for k, v := range r.Headers {
			out.Headers[k] = v
		}
	}
	if r.Query != nil {
		out.Query = make(map[string]string, len(r.Query))
		for k, v := range r.Query {
			out.Query[k] = v
		}
	}
	return out
}
