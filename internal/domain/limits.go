package domain

import "unicode/utf8"

// Limits pinned in CONTRACTS.md section 2. Both SDK and server use these numbers.
const (
	MaxBatchEvents     = 50
	MaxBodyBytes       = 1024 * 1024
	MaxTags            = 50
	MaxTagKeyLength    = 32
	MaxTagValueLength  = 200
	MaxBreadcrumbs     = 100
	MaxExceptionDepth  = 5 // total chain length: the root plus at most 4 causes
	MaxFrames          = 100
	MaxMessageLength   = 8192
	ContextLines       = 5
	MaxContextLineLen  = 300
	DefaultBreadcrumbs = 50
)

// Ellipsis is appended to truncated strings.
const Ellipsis = "…"

// Truncate shortens s to at most max characters (runes), including the trailing ellipsis.
func Truncate(s string, max int) string {
	if max <= 0 {
		return ""
	}
	if len(s) <= max || utf8.RuneCountInString(s) <= max {
		return s
	}
	n := 0
	for i := range s {
		if n == max-1 {
			return s[:i] + Ellipsis
		}
		n++
	}
	return s
}

// TruncateBytes cuts s to at most max bytes without splitting a UTF-8 sequence.
func TruncateBytes(s string, max int) string {
	if len(s) <= max {
		return s
	}
	for max > 0 && !utf8.RuneStart(s[max]) {
		max--
	}
	return s[:max]
}
