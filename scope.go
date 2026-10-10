package faultline

import (
	"net"
	"net/http"
	"strings"

	"github.com/getfaultline/faultline-go/internal/application"
	"github.com/getfaultline/faultline-go/internal/domain"
)

// Scope holds contextual data (tags, user, contexts, extra, breadcrumbs, request, level,
// fingerprint) applied to events captured with it. It is safe for concurrent use, and every
// method is a no-op on a nil *Scope.
type Scope struct {
	s *domain.Scope
}

func (sc *Scope) ok() bool { return sc != nil && sc.s != nil }

func (sc *Scope) guard() {
	if r := recover(); r != nil {
		logInternal(r)
	}
}

// SetTag sets a searchable tag.
func (sc *Scope) SetTag(key, value string) {
	defer sc.guard()
	if sc.ok() {
		sc.s.SetTag(key, value)
	}
}

// SetTags merges tags.
func (sc *Scope) SetTags(tags map[string]string) {
	defer sc.guard()
	if sc.ok() {
		sc.s.SetTags(tags)
	}
}

// SetUser sets the user. The zero User clears it.
func (sc *Scope) SetUser(u User) {
	defer sc.guard()
	if sc.ok() {
		sc.s.SetUser(u)
	}
}

// SetContext sets structured context. A nil map removes it.
func (sc *Scope) SetContext(name string, ctx map[string]any) {
	defer sc.guard()
	if sc.ok() {
		sc.s.SetContext(name, ctx)
	}
}

// SetExtra sets arbitrary extra data. Values are normalized to JSON when an event is built.
func (sc *Scope) SetExtra(key string, value any) {
	defer sc.guard()
	if sc.ok() {
		sc.s.SetExtra(key, value)
	}
}

// SetExtras merges extra data.
func (sc *Scope) SetExtras(extras map[string]any) {
	defer sc.guard()
	if sc.ok() {
		sc.s.SetExtras(extras)
	}
}

// SetLevel overrides the level of events captured with this scope ("" clears it).
func (sc *Scope) SetLevel(level Level) {
	defer sc.guard()
	if sc.ok() {
		sc.s.SetLevel(level)
	}
}

// SetFingerprint overrides grouping for events captured with this scope (nil clears it).
func (sc *Scope) SetFingerprint(fingerprint []string) {
	defer sc.guard()
	if sc.ok() {
		sc.s.SetFingerprint(fingerprint)
	}
}

// SetRequest attaches request data.
func (sc *Scope) SetRequest(r Request) {
	defer sc.guard()
	if sc.ok() {
		sc.s.SetRequest(&r)
	}
}

// SetHTTPRequest attaches method, URL, headers and query of r. When SendDefaultPII is on it
// also records the client IP (from RemoteAddr) as user.ipAddress.
func (sc *Scope) SetHTTPRequest(r *http.Request) {
	defer sc.guard()
	if !sc.ok() || r == nil {
		return
	}
	req := RequestFromHTTP(r)
	sc.s.SetRequest(&req)
	if c := hub.Client(); c != nil && c.Options().SendDefaultPII {
		if ip := remoteIP(r.RemoteAddr); ip != "" {
			u := sc.s.User()
			u.IPAddress = ip
			sc.s.SetUser(u)
		}
	}
}

// AddBreadcrumb records a breadcrumb, keeping at most Options.MaxBreadcrumbs.
func (sc *Scope) AddBreadcrumb(b Breadcrumb) {
	defer sc.guard()
	if sc.ok() {
		hub.AddBreadcrumb(b, sc.s)
	}
}

// ClearBreadcrumbs removes all breadcrumbs.
func (sc *Scope) ClearBreadcrumbs() {
	defer sc.guard()
	if sc.ok() {
		sc.s.ClearBreadcrumbs()
	}
}

// Clear resets the scope.
func (sc *Scope) Clear() {
	defer sc.guard()
	if sc.ok() {
		sc.s.Clear()
	}
}

// CaptureException captures err with this scope (the current scope if sc is nil).
func (sc *Scope) CaptureException(err error) (id *EventID) {
	defer func() {
		if r := recover(); r != nil {
			logInternal(r)
			id = nil
		}
	}()
	if err == nil {
		return nil
	}
	return toEventID(hub.CaptureException(err, application.Hint{PCs: callers(0)}, sc.inner()))
}

// CaptureMessage captures a message with this scope (the current scope if sc is nil).
func (sc *Scope) CaptureMessage(message string, level Level) (id *EventID) {
	defer func() {
		if r := recover(); r != nil {
			logInternal(r)
			id = nil
		}
	}()
	return toEventID(hub.CaptureMessage(message, level, sc.inner()))
}

func (sc *Scope) inner() *domain.Scope {
	if sc.ok() {
		return sc.s
	}
	return nil
}

// RequestFromHTTP extracts method, absolute URL, headers and query parameters from r.
// Multiple values are joined with ", ". Scrubbing happens later, when the event is built.
func RequestFromHTTP(r *http.Request) Request {
	var out Request
	if r == nil {
		return out
	}
	out.Method = r.Method
	if r.URL != nil {
		scheme := "http"
		if r.TLS != nil {
			scheme = "https"
		}
		host := r.Host
		if host == "" {
			host = r.URL.Host
		}
		if host != "" {
			out.URL = scheme + "://" + host + r.URL.RequestURI()
		} else {
			out.URL = r.URL.RequestURI()
		}
		if q := r.URL.Query(); len(q) > 0 {
			out.Query = make(map[string]string, len(q))
			for k, v := range q {
				out.Query[k] = strings.Join(v, ", ")
			}
		}
	}
	if len(r.Header) > 0 {
		out.Headers = make(map[string]string, len(r.Header)+1)
		for k, v := range r.Header {
			out.Headers[k] = strings.Join(v, ", ")
		}
		if r.Host != "" {
			out.Headers["Host"] = r.Host
		}
	}
	return out
}

func remoteIP(addr string) string {
	if addr == "" {
		return ""
	}
	if host, _, err := net.SplitHostPort(addr); err == nil {
		return host
	}
	return addr
}
