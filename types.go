package faultline

import "github.com/getfaultline/faultline-go/internal/domain"

// Event is the payload sent to the ingest API (see CONTRACTS.md section 2). BeforeSend
// receives and returns it.
type Event = domain.Event

// Exception is one error of a cause chain.
type Exception = domain.Exception

// Stacktrace holds frames ordered oldest to newest.
type Stacktrace = domain.Stacktrace

// StackFrame is one frame of a stack trace.
type StackFrame = domain.StackFrame

// User identifies the affected user. The zero value clears it.
type User = domain.User

// Request describes the HTTP request being handled.
type Request = domain.Request

// Breadcrumb is a trail entry recorded before an event. Timestamp defaults to now and Type
// to BreadcrumbDefault.
type Breadcrumb = domain.Breadcrumb

// BreadcrumbType is the kind of a breadcrumb.
type BreadcrumbType = domain.BreadcrumbType

// Level is the severity of an event or breadcrumb.
type Level = domain.Level

// Levels.
const (
	LevelFatal   = domain.LevelFatal
	LevelError   = domain.LevelError
	LevelWarning = domain.LevelWarning
	LevelInfo    = domain.LevelInfo
	LevelDebug   = domain.LevelDebug
)

// Breadcrumb types.
const (
	BreadcrumbDefault    = domain.BreadcrumbDefault
	BreadcrumbHTTP       = domain.BreadcrumbHTTP
	BreadcrumbLog        = domain.BreadcrumbLog
	BreadcrumbQuery      = domain.BreadcrumbQuery
	BreadcrumbNavigation = domain.BreadcrumbNavigation
)

// EventID identifies a captured event (uuid v4).
type EventID string

func toEventID(id string) *EventID {
	if id == "" {
		return nil
	}
	e := EventID(id)
	return &e
}
