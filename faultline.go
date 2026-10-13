// Package faultline is the Go SDK for Faultline, a self-hostable error tracker.
//
// It captures errors, panics and messages, adds context (stack traces with source,
// breadcrumbs, request data, user, tags), scrubs secrets, and sends batched, gzip-compressed
// events to the Faultline ingest API from a background goroutine.
//
//	err := faultline.Init(faultline.Options{
//		APIKey:      os.Getenv("FAULTLINE_API_KEY"),
//		Environment: "production",
//		Release:     "api@1.4.2",
//	})
//	if err != nil {
//		log.Printf("faultline: %v", err)
//	}
//	defer faultline.Close(2 * time.Second)
//	defer faultline.Recover()
//
// Every exported function is safe to call before Init, after Close, concurrently, and with
// nil or zero arguments: the SDK never panics into the host application. Internal problems
// are logged to stderr only when Options.Debug is true.
package faultline

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/getfaultline/faultline-go/internal/application"
	"github.com/getfaultline/faultline-go/internal/domain"
	"github.com/getfaultline/faultline-go/internal/infrastructure/source"
	"github.com/getfaultline/faultline-go/internal/infrastructure/stack"
	"github.com/getfaultline/faultline-go/internal/infrastructure/sysinfo"
	"github.com/getfaultline/faultline-go/internal/infrastructure/transport"
)

// SDKVersion is the version reported in event.sdk.version.
const SDKVersion = "0.1.1"

// DefaultEndpoint is the hosted Faultline server.
const DefaultEndpoint = domain.DefaultEndpoint

// ErrInvalidOptions is wrapped by the error Init returns when options are invalid.
var ErrInvalidOptions = errors.New("invalid options")

// Options configures the SDK. The zero value of every field except APIKey is a usable default.
type Options struct {
	// APIKey is the public ingest key (flt_ingest_...). Required unless Enabled is false.
	APIKey string
	// Endpoint is the base URL of the Faultline server (default https://api.faultsline.dev).
	// Events are posted to {Endpoint}/api/v1/ingest/events.
	Endpoint string
	// Environment, e.g. "production" or "staging".
	Environment string
	// Release, e.g. "api@1.4.2" or a git sha.
	Release string
	// ServerName is reported as event.serverName (default: os.Hostname()).
	ServerName string
	// SampleRate is the fraction of events to send, in (0, 1]. 0 means 1 (send everything);
	// use Enabled to turn the SDK off.
	SampleRate float64
	// MaxBreadcrumbs kept per scope: 0 means 50, values above 100 are capped, negative
	// disables breadcrumbs.
	MaxBreadcrumbs int
	// SendDefaultPII keeps the user IP and the cookie, set-cookie, authorization,
	// proxy-authorization, x-forwarded-for, x-real-ip and forwarded headers. Values under
	// sensitive keys are always filtered.
	SendDefaultPII bool
	// BeforeSend may modify an event or drop it by returning nil. It runs after scrubbing.
	// If it panics, the event is dropped.
	BeforeSend func(*Event) *Event
	// IgnoreErrors are regular expressions matched against the exception type, its message,
	// "type: message", and the message of message events. A plain string without regexp
	// metacharacters therefore matches as a substring.
	IgnoreErrors []string
	// Enabled turns the SDK on or off (default on). Use faultline.Bool(false) to disable it.
	Enabled *bool
	// Debug logs SDK internals (dropped events, retries, invalid options) to stderr.
	Debug bool
	// SwallowPanics makes Recover and RecoverWithContext swallow the panic after capturing
	// it. By default they flush for up to 2s and re-panic with the original value, so the
	// program behaves exactly as it would without the SDK.
	SwallowPanics bool
}

// Bool returns a pointer to v, for Options.Enabled.
func Bool(v bool) *bool { return &v }

var (
	hub         = application.NewHub()
	stackParser = stack.New()
)

// Init configures the SDK. Calling it again replaces the previous client (which is closed in
// the background). It returns a non-nil error wrapping ErrInvalidOptions when options are
// invalid: invalid optional values fall back to their defaults and the SDK still runs, while a
// missing APIKey or an unusable Endpoint leaves the SDK disabled (see IsEnabled). Init never
// panics.
func Init(opts Options) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("faultline: internal error during Init: %v", r)
		}
	}()
	if prev := hub.BindClient(nil); prev != nil {
		go prev.Close(2 * time.Second)
	}

	resolved, warnings, errs := domain.ResolveOptions(domain.RawOptions{
		APIKey:         opts.APIKey,
		Endpoint:       opts.Endpoint,
		Environment:    opts.Environment,
		Release:        opts.Release,
		ServerName:     opts.ServerName,
		SampleRate:     opts.SampleRate,
		MaxBreadcrumbs: opts.MaxBreadcrumbs,
		SendDefaultPII: opts.SendDefaultPII,
		BeforeSend:     opts.BeforeSend,
		IgnoreErrors:   opts.IgnoreErrors,
		Enabled:        opts.Enabled,
		Debug:          opts.Debug,
		SwallowPanics:  opts.SwallowPanics,
	})
	logger := sysinfo.NewLogger(opts.Debug, nil)
	for _, w := range warnings {
		logger.Warnf("invalid option: %s", w)
	}
	problems := append(append([]string(nil), errs...), warnings...)
	var optErr error
	if len(problems) > 0 {
		optErr = fmt.Errorf("faultline: %w: %s", ErrInvalidOptions, strings.Join(problems, "; "))
	}
	if len(errs) > 0 {
		logger.Errorf("SDK disabled: %s", strings.Join(errs, "; "))
		return optErr
	}
	if !resolved.Enabled {
		logger.Debugf("SDK disabled (Enabled: false)")
		return optErr
	}

	t := transport.New(transport.Options{
		Endpoint:  resolved.Endpoint,
		APIKey:    resolved.APIKey,
		Logger:    logger,
		UserAgent: domain.SDKName + "/" + SDKVersion,
	})
	client := application.NewClient(resolved, application.Deps{
		Transport:  t,
		Stack:      stackParser,
		Source:     source.New(source.Options{}),
		Clock:      sysinfo.Clock{},
		IDs:        sysinfo.IDs{},
		Random:     sysinfo.Random{},
		Logger:     logger,
		Runtime:    &sysinfo.Runtime{},
		SDKVersion: SDKVersion,
	})
	hub.BindClient(client)
	logger.Debugf("initialized (endpoint %s, sdk %s)", resolved.Endpoint, SDKVersion)
	return optErr
}

// IsEnabled reports whether Init succeeded and the SDK is sending events.
func IsEnabled() (enabled bool) {
	defer func() {
		if recover() != nil {
			enabled = false
		}
	}()
	c := hub.Client()
	return c != nil && c.Enabled()
}

// Flush waits until queued events are sent. It returns false on timeout.
func Flush(timeout time.Duration) (ok bool) {
	defer func() {
		if recover() != nil {
			ok = false
		}
	}()
	return hub.Flush(timeout)
}

// Close flushes for up to timeout and disables the SDK. It returns false on timeout.
func Close(timeout time.Duration) (ok bool) {
	defer func() {
		if recover() != nil {
			ok = false
		}
	}()
	return hub.Close(timeout)
}
