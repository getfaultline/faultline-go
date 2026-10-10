package domain

import (
	"encoding/json"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

func sanitizeLines(lines []string, keepLast bool) []string {
	if len(lines) == 0 {
		return nil
	}
	if len(lines) > ContextLines {
		if keepLast {
			lines = lines[len(lines)-ContextLines:]
		} else {
			lines = lines[:ContextLines]
		}
	}
	out := make([]string, len(lines))
	for i, l := range lines {
		out[i] = Truncate(l, MaxContextLineLen)
	}
	return out
}

func sanitizeFrame(f StackFrame) StackFrame {
	out := StackFrame{
		Filename: f.Filename,
		InApp:    f.InApp,
	}
	if out.Filename == "" {
		out.Filename = "<unknown>"
	}
	out.Filename = Truncate(out.Filename, 1024)
	out.Function = Truncate(f.Function, 1024)
	out.Module = Truncate(f.Module, 1024)
	if f.Lineno > 0 {
		out.Lineno = f.Lineno
	}
	if f.Colno > 0 {
		out.Colno = f.Colno
	}
	out.PreContext = sanitizeLines(f.PreContext, true)
	out.ContextLine = Truncate(f.ContextLine, MaxContextLineLen)
	out.PostContext = sanitizeLines(f.PostContext, false)
	return out
}

func sanitizeException(ex *Exception, depth int) *Exception {
	if ex == nil || depth >= MaxExceptionDepth {
		return nil
	}
	out := &Exception{
		Type:  Truncate(ex.Type, 256),
		Value: Truncate(ex.Value, MaxMessageLength),
	}
	if out.Type == "" {
		out.Type = "error"
	}
	frames := ex.Stacktrace.Frames
	if len(frames) > MaxFrames {
		frames = frames[len(frames)-MaxFrames:]
	}
	out.Stacktrace.Frames = make([]StackFrame, len(frames))
	for i, f := range frames {
		out.Stacktrace.Frames[i] = sanitizeFrame(f)
	}
	out.Cause = sanitizeException(ex.Cause, depth+1)
	return out
}

func sanitizeBreadcrumb(b Breadcrumb, now time.Time) Breadcrumb {
	out := Breadcrumb{
		Timestamp: b.Timestamp,
		Type:      b.Type,
		Category:  Truncate(b.Category, 256),
		Message:   Truncate(b.Message, MaxMessageLength),
		Data:      NormalizeMap(b.Data),
	}
	if out.Timestamp.IsZero() {
		out.Timestamp = now
	}
	if !out.Type.Valid() {
		out.Type = BreadcrumbDefault
	}
	if b.Level.Valid() {
		out.Level = b.Level
	}
	return out
}

func sanitizeTags(tags map[string]string) map[string]string {
	if len(tags) == 0 {
		return nil
	}
	keys := make([]string, 0, len(tags))
	for k := range tags {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make(map[string]string, len(tags))
	for _, k := range keys {
		if len(out) >= MaxTags {
			break
		}
		key := k
		if utf8.RuneCountInString(key) > MaxTagKeyLength {
			// Keys are cut, not ellipsized, to match the Node SDK (slice(0, 32)).
			key = string([]rune(key)[:MaxTagKeyLength])
		}
		if key == "" {
			continue
		}
		out[key] = Truncate(tags[k], MaxTagValueLength)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func sanitizeStringMap(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		if utf8.RuneCountInString(k) > 256 { // the server caps header/query keys at 256
			k = string([]rune(k)[:256])
		}
		out[k] = Truncate(v, MaxMessageLength)
	}
	return out
}

// SanitizeEvent coerces an event (possibly modified by BeforeSend) into a payload that
// satisfies the contract: valid enums, required fields present, every pinned limit enforced,
// arbitrary values normalized to JSON-safe data. It returns a new event, or nil when the
// input is unusable (nil or missing an event id).
func SanitizeEvent(e *Event, sdkVersion string, now time.Time) *Event {
	if e == nil || e.EventID == "" {
		return nil
	}
	out := &Event{
		EventID:     e.EventID,
		Timestamp:   e.Timestamp,
		Level:       e.Level,
		Platform:    Platform,
		Environment: Truncate(e.Environment, 200),
		Release:     Truncate(e.Release, 200),
		ServerName:  Truncate(e.ServerName, 200),
		Message:     Truncate(e.Message, MaxMessageLength),
		Exception:   sanitizeException(e.Exception, 0),
		Tags:        sanitizeTags(e.Tags),
		Contexts:    NormalizeMap(e.Contexts),
		Extra:       NormalizeMap(e.Extra),
		SDK:         SDKInfo{Name: SDKName, Version: sdkVersion},
	}
	if out.Timestamp.IsZero() {
		out.Timestamp = now
	}
	if !out.Level.Valid() {
		out.Level = LevelError
	}
	for _, f := range e.Fingerprint {
		out.Fingerprint = append(out.Fingerprint, Truncate(f, 512))
	}
	if e.User != nil {
		u := User{
			ID:        Truncate(e.User.ID, 256),
			Email:     Truncate(e.User.Email, 256),
			Username:  Truncate(e.User.Username, 256),
			IPAddress: Truncate(e.User.IPAddress, 256),
		}
		if !u.IsZero() {
			out.User = &u
		}
	}
	if e.Request != nil {
		r := Request{
			Method:  Truncate(e.Request.Method, 16),
			URL:     Truncate(e.Request.URL, 4096),
			Headers: sanitizeStringMap(e.Request.Headers),
			Query:   sanitizeStringMap(e.Request.Query),
		}
		if !r.isZero() {
			out.Request = &r
		}
	}
	crumbs := e.Breadcrumbs
	if len(crumbs) > MaxBreadcrumbs {
		crumbs = crumbs[len(crumbs)-MaxBreadcrumbs:]
	}
	for _, b := range crumbs {
		out.Breadcrumbs = append(out.Breadcrumbs, sanitizeBreadcrumb(b, now))
	}
	return out
}

// envelopeBytes is the size of `{"events":[]}`.
const envelopeBytes = len(`{"events":[]}`)

// EnvelopeBytes is the size of the empty batch envelope.
func EnvelopeBytes() int { return envelopeBytes }

// SerializeWithinLimit marshals e, trimming breadcrumbs, then source context, then extra
// until it fits in maxBytes. It returns false when the event still does not fit.
func SerializeWithinLimit(e *Event, maxBytes int) ([]byte, bool) {
	attempt := func(ev *Event) ([]byte, bool) {
		raw, err := json.Marshal(ev)
		if err != nil || len(raw) > maxBytes {
			return nil, false
		}
		return raw, true
	}
	if raw, ok := attempt(e); ok {
		return raw, true
	}
	cur := *e
	cur.Breadcrumbs = nil
	if raw, ok := attempt(&cur); ok {
		return raw, true
	}
	cur.Exception = stripContext(cur.Exception)
	if raw, ok := attempt(&cur); ok {
		return raw, true
	}
	cur.Extra = nil
	return attempt(&cur)
}

func stripContext(ex *Exception) *Exception {
	if ex == nil {
		return nil
	}
	out := &Exception{Type: ex.Type, Value: ex.Value, Cause: stripContext(ex.Cause)}
	out.Stacktrace.Frames = make([]StackFrame, len(ex.Stacktrace.Frames))
	for i, f := range ex.Stacktrace.Frames {
		f.PreContext, f.ContextLine, f.PostContext = nil, "", nil
		out.Stacktrace.Frames[i] = f
	}
	return out
}

// DedupeKey is the stable key used to detect identical consecutive events.
func DedupeKey(e *Event) string {
	if len(e.Fingerprint) > 0 {
		return "fp:" + strings.Join(e.Fingerprint, "|")
	}
	if ex := e.Exception; ex != nil {
		frames := ex.Stacktrace.Frames
		if len(frames) > 5 {
			frames = frames[len(frames)-5:]
		}
		parts := make([]string, len(frames))
		for i, f := range frames {
			fn := f.Function
			if fn == "" {
				fn = "?"
			}
			parts[i] = f.Filename + ":" + fn + ":" + strconv.Itoa(f.Lineno) + ":" + strconv.Itoa(f.Colno)
		}
		return "ex:" + ex.Type + ":" + ex.Value + ":" + strings.Join(parts, ",")
	}
	return "msg:" + string(e.Level) + ":" + e.Message
}

// MatchesIgnoreErrors reports whether any pattern matches the exception type, its value,
// "type: value", or (for message events) the message.
func MatchesIgnoreErrors(e *Event, patterns []*regexp.Regexp) bool {
	if len(patterns) == 0 {
		return false
	}
	var candidates []string
	if ex := e.Exception; ex != nil {
		candidates = append(candidates, ex.Type, ex.Value, ex.Type+": "+ex.Value)
	} else if e.Message != "" {
		candidates = append(candidates, e.Message)
	}
	for _, p := range patterns {
		if p == nil {
			continue
		}
		for _, c := range candidates {
			if p.MatchString(c) {
				return true
			}
		}
	}
	return false
}
