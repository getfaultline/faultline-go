// Package domain holds the pure core of the SDK: the event payload contract (CONTRACTS.md
// section 2), limits, scrubbing, sanitization, options validation and the Scope data
// structure. It performs no I/O and imports only pure standard-library packages.
package domain

import (
	"encoding/json"
	"time"
)

// Level is the severity of an event or breadcrumb.
type Level string

// Levels accepted by the ingest API.
const (
	LevelFatal   Level = "fatal"
	LevelError   Level = "error"
	LevelWarning Level = "warning"
	LevelInfo    Level = "info"
	LevelDebug   Level = "debug"
)

// Valid reports whether l is one of the five contract levels.
func (l Level) Valid() bool {
	switch l {
	case LevelFatal, LevelError, LevelWarning, LevelInfo, LevelDebug:
		return true
	}
	return false
}

// BreadcrumbType is the kind of a breadcrumb.
type BreadcrumbType string

// Breadcrumb types accepted by the ingest API.
const (
	BreadcrumbDefault    BreadcrumbType = "default"
	BreadcrumbHTTP       BreadcrumbType = "http"
	BreadcrumbLog        BreadcrumbType = "log"
	BreadcrumbQuery      BreadcrumbType = "query"
	BreadcrumbNavigation BreadcrumbType = "navigation"
)

// Valid reports whether t is one of the five contract breadcrumb types.
func (t BreadcrumbType) Valid() bool {
	switch t {
	case BreadcrumbDefault, BreadcrumbHTTP, BreadcrumbLog, BreadcrumbQuery, BreadcrumbNavigation:
		return true
	}
	return false
}

// Contract constants for this SDK (CONTRACTS.md section 7).
const (
	Platform = "go"
	SDKName  = "faultline-go"
)

// StackFrame is one frame of a stack trace.
type StackFrame struct {
	Filename    string   `json:"filename"`
	Function    string   `json:"function,omitempty"`
	Module      string   `json:"module,omitempty"`
	Lineno      int      `json:"lineno,omitempty"`
	Colno       int      `json:"colno,omitempty"`
	InApp       bool     `json:"inApp"`
	PreContext  []string `json:"preContext,omitempty"`
	ContextLine string   `json:"contextLine,omitempty"`
	PostContext []string `json:"postContext,omitempty"`
}

// Stacktrace holds frames ordered oldest to newest.
type Stacktrace struct {
	Frames []StackFrame `json:"frames"`
}

// Exception describes one error in a cause chain.
type Exception struct {
	Type       string     `json:"type"`
	Value      string     `json:"value"`
	Stacktrace Stacktrace `json:"stacktrace"`
	Cause      *Exception `json:"cause,omitempty"`
}

// User identifies the user affected by an event.
type User struct {
	ID        string `json:"id,omitempty"`
	Email     string `json:"email,omitempty"`
	Username  string `json:"username,omitempty"`
	IPAddress string `json:"ipAddress,omitempty"`
}

// IsZero reports whether no field is set.
func (u User) IsZero() bool { return u == User{} }

// Request describes the HTTP request being handled when the event happened.
type Request struct {
	Method  string            `json:"method,omitempty"`
	URL     string            `json:"url,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
	Query   map[string]string `json:"query,omitempty"`
}

func (r Request) isZero() bool {
	return r.Method == "" && r.URL == "" && len(r.Headers) == 0 && len(r.Query) == 0
}

// Breadcrumb is a trail entry recorded before an event.
type Breadcrumb struct {
	Timestamp time.Time      `json:"timestamp"`
	Type      BreadcrumbType `json:"type"`
	Category  string         `json:"category,omitempty"`
	Message   string         `json:"message,omitempty"`
	Level     Level          `json:"level,omitempty"`
	Data      map[string]any `json:"data,omitempty"`
}

// MarshalJSON renders the timestamp as an ISO 8601 UTC string with millisecond precision.
func (b Breadcrumb) MarshalJSON() ([]byte, error) {
	type plain Breadcrumb
	return json.Marshal(struct {
		plain
		Timestamp string `json:"timestamp"`
	}{plain(b), FormatTimestamp(b.Timestamp)})
}

// SDKInfo identifies the SDK that produced an event.
type SDKInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// Event is the payload sent to POST /api/v1/ingest/events.
type Event struct {
	EventID     string            `json:"eventId"`
	Timestamp   time.Time         `json:"timestamp"`
	Level       Level             `json:"level"`
	Platform    string            `json:"platform"`
	Environment string            `json:"environment,omitempty"`
	Release     string            `json:"release,omitempty"`
	ServerName  string            `json:"serverName,omitempty"`
	Message     string            `json:"message,omitempty"`
	Exception   *Exception        `json:"exception,omitempty"`
	Fingerprint []string          `json:"fingerprint,omitempty"`
	Tags        map[string]string `json:"tags,omitempty"`
	User        *User             `json:"user,omitempty"`
	Request     *Request          `json:"request,omitempty"`
	Contexts    map[string]any    `json:"contexts,omitempty"`
	Breadcrumbs []Breadcrumb      `json:"breadcrumbs,omitempty"`
	Extra       map[string]any    `json:"extra,omitempty"`
	SDK         SDKInfo           `json:"sdk"`
}

// MarshalJSON renders the timestamp as an ISO 8601 UTC string with millisecond precision.
func (e Event) MarshalJSON() ([]byte, error) {
	type plain Event
	return json.Marshal(struct {
		plain
		Timestamp string `json:"timestamp"`
	}{plain(e), FormatTimestamp(e.Timestamp)})
}

// FormatTimestamp formats t like JavaScript's Date.toISOString: UTC, milliseconds, "Z".
func FormatTimestamp(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05.000Z")
}
