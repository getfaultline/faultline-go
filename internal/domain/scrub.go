package domain

import (
	"net/url"
	"regexp"
	"strings"
)

// Filtered replaces scrubbed values.
const Filtered = "[Filtered]"

// Regex sources copied verbatim from CONTRACTS.md section 2 (the JavaScript /i flag becomes (?i)).
const (
	SensitiveKeyPattern = `(authorization|cookie|set-cookie|password|passwd|secret|token|api[_-]?key|x-faultline-key)`
	CreditCardPattern   = `\b(?:\d[ -]*?){13,16}\b`
)

var (
	sensitiveKeyRegex = regexp.MustCompile(`(?i)` + SensitiveKeyPattern)
	creditCardRegex   = regexp.MustCompile(CreditCardPattern)
	queryParamRegex   = regexp.MustCompile(`([?&])([^=&#]+)=([^&#]*)`)
)

// PIIHeaders are removed entirely when SendDefaultPII is false (lowercase).
var PIIHeaders = []string{
	"cookie",
	"set-cookie",
	"authorization",
	"proxy-authorization",
	"x-forwarded-for",
	"x-real-ip",
	"forwarded",
}

// IsSensitiveKey reports whether values under key must be filtered.
func IsSensitiveKey(key string) bool { return sensitiveKeyRegex.MatchString(key) }

// LooksLikeCreditCard reports whether value contains something shaped like a card number.
func LooksLikeCreditCard(value string) bool { return creditCardRegex.MatchString(value) }

func isPIIHeader(name string) bool {
	lower := strings.ToLower(name)
	for _, h := range PIIHeaders {
		if h == lower {
			return true
		}
	}
	return false
}

// ScrubURL masks sensitive query parameters ("?token=abc" becomes "?token=[Filtered]").
func ScrubURL(value string) string {
	if !strings.Contains(value, "?") || !strings.Contains(value, "=") {
		return value
	}
	matches := queryParamRegex.FindAllStringSubmatchIndex(value, -1)
	if len(matches) == 0 {
		return value
	}
	var b strings.Builder
	last := 0
	for _, m := range matches {
		key := value[m[4]:m[5]]
		decoded, err := url.QueryUnescape(key)
		if err != nil {
			decoded = key
		}
		if !IsSensitiveKey(decoded) {
			continue
		}
		b.WriteString(value[last:m[6]])
		b.WriteString(Filtered)
		last = m[7]
	}
	b.WriteString(value[last:])
	return b.String()
}

// ScrubOptions controls ScrubValue.
type ScrubOptions struct {
	// CreditCards also replaces card-like strings and masks sensitive URL query parameters.
	CreditCards bool
}

// ScrubValue deep-scrubs a normalized (JSON-shaped) value: values under sensitive keys become
// "[Filtered]"; optionally card-like strings are filtered and URLs get sensitive params masked.
// It returns a new value and never mutates the input.
func ScrubValue(value any, opts ScrubOptions) any { return scrubValue(value, opts, 0) }

func scrubValue(value any, opts ScrubOptions, depth int) any {
	if depth > 20 {
		return value
	}
	switch v := value.(type) {
	case string:
		if opts.CreditCards {
			if LooksLikeCreditCard(v) {
				return Filtered
			}
			return ScrubURL(v)
		}
		return v
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = scrubValue(item, opts, depth+1)
		}
		return out
	case map[string]any:
		return scrubMap(v, opts, depth)
	case map[string]string:
		out := make(map[string]any, len(v))
		for k, item := range v {
			out[k] = item
		}
		return scrubMap(out, opts, depth)
	}
	return value
}

func scrubMap(m map[string]any, opts ScrubOptions, depth int) map[string]any {
	if m == nil {
		return nil
	}
	out := make(map[string]any, len(m))
	for k, item := range m {
		if IsSensitiveKey(k) {
			out[k] = Filtered
		} else {
			out[k] = scrubValue(item, opts, depth+1)
		}
	}
	return out
}

func scrubStringMap(m map[string]string, opts ScrubOptions) map[string]string {
	if m == nil {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		switch {
		case IsSensitiveKey(k):
			out[k] = Filtered
		case opts.CreditCards && LooksLikeCreditCard(v):
			out[k] = Filtered
		case opts.CreditCards:
			out[k] = ScrubURL(v)
		default:
			out[k] = v
		}
	}
	return out
}

// ScrubEvent applies client-side scrubbing (CONTRACTS.md section 2) and returns a new event.
// Sensitive keys anywhere in request/extra/contexts/breadcrumb data become "[Filtered]";
// card-like values in request/extra/breadcrumb data become "[Filtered]". When sendDefaultPII
// is false the user IP and cookie/auth/forwarding headers are removed entirely.
func ScrubEvent(e *Event, sendDefaultPII bool) *Event {
	if e == nil {
		return nil
	}
	out := *e
	cc := ScrubOptions{CreditCards: true}

	if e.Request != nil {
		req := *e.Request
		if req.Headers != nil {
			headers := make(map[string]string, len(req.Headers))
			for k, v := range req.Headers {
				if !sendDefaultPII && isPIIHeader(k) {
					continue
				}
				if IsSensitiveKey(k) {
					headers[k] = Filtered
				} else {
					headers[k] = v
				}
			}
			req.Headers = headers
		}
		req.Query = scrubStringMap(req.Query, cc)
		if req.URL != "" {
			req.URL = ScrubURL(req.URL)
		}
		out.Request = &req
	}

	if e.User != nil {
		user := *e.User
		if !sendDefaultPII {
			user.IPAddress = ""
		}
		if user.IsZero() {
			out.User = nil
		} else {
			out.User = &user
		}
	}

	if e.Tags != nil {
		tags := make(map[string]string, len(e.Tags))
		for k, v := range e.Tags {
			if IsSensitiveKey(k) {
				tags[k] = Filtered
			} else {
				tags[k] = v
			}
		}
		out.Tags = tags
	}

	out.Extra = scrubMap(e.Extra, cc, 0)
	out.Contexts = scrubMap(e.Contexts, ScrubOptions{}, 0)

	if e.Breadcrumbs != nil {
		crumbs := make([]Breadcrumb, len(e.Breadcrumbs))
		for i, b := range e.Breadcrumbs {
			if b.Data != nil {
				b.Data = scrubMap(b.Data, cc, 0)
			}
			crumbs[i] = b
		}
		out.Breadcrumbs = crumbs
	}
	return &out
}
