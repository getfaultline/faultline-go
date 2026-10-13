package domain

import (
	"fmt"
	"math"
	"net/url"
	"regexp"
	"strings"
)

// DefaultEndpoint is the hosted Faultline server.
const DefaultEndpoint = "https://api.faultsline.dev"

// IngestPath is appended to the endpoint.
const IngestPath = "/api/v1/ingest/events"

// BeforeSend may modify an event or drop it by returning nil.
type BeforeSend func(*Event) *Event

// RawOptions is what the user passed to Init, before validation.
type RawOptions struct {
	APIKey         string
	Endpoint       string
	Environment    string
	Release        string
	ServerName     string
	SampleRate     float64
	MaxBreadcrumbs int
	SendDefaultPII bool
	BeforeSend     BeforeSend
	IgnoreErrors   []string
	Enabled        *bool
	Debug          bool
	SwallowPanics  bool
}

// Options are validated, defaulted options.
type Options struct {
	APIKey         string
	Endpoint       string
	Environment    string
	Release        string
	ServerName     string
	SampleRate     float64
	MaxBreadcrumbs int
	SendDefaultPII bool
	BeforeSend     BeforeSend
	IgnoreErrors   []*regexp.Regexp
	Enabled        bool
	Debug          bool
	SwallowPanics  bool
}

// ResolveOptions validates raw options. Invalid optional values fall back to their default
// and produce a warning; only fatal problems (missing APIKey while enabled, unusable
// Endpoint) produce errors, in which case the SDK must stay disabled. It never panics.
func ResolveOptions(raw RawOptions) (opts Options, warnings, errs []string) {
	opts = Options{
		Endpoint:       DefaultEndpoint,
		Environment:    limitOpt(raw.Environment),
		Release:        limitOpt(raw.Release),
		ServerName:     limitOpt(raw.ServerName),
		SampleRate:     1,
		MaxBreadcrumbs: DefaultBreadcrumbs,
		SendDefaultPII: raw.SendDefaultPII,
		BeforeSend:     raw.BeforeSend,
		Enabled:        raw.Enabled == nil || *raw.Enabled,
		Debug:          raw.Debug,
		SwallowPanics:  raw.SwallowPanics,
	}

	opts.APIKey = strings.TrimSpace(raw.APIKey)
	if opts.APIKey == "" && opts.Enabled {
		errs = append(errs, "APIKey is required")
	}

	if raw.Endpoint != "" {
		if ep, ok := parseEndpoint(raw.Endpoint); ok {
			opts.Endpoint = ep
		} else {
			errs = append(errs, "Endpoint must be an absolute http(s) URL")
		}
	}

	switch {
	case raw.SampleRate == 0:
		// zero value: use the default (send everything); disable the SDK with Enabled instead
	case math.IsNaN(raw.SampleRate) || raw.SampleRate < 0 || raw.SampleRate > 1:
		warnings = append(warnings, "SampleRate must be between 0 and 1; using 1")
	default:
		opts.SampleRate = raw.SampleRate
	}

	switch {
	case raw.MaxBreadcrumbs == 0:
	case raw.MaxBreadcrumbs < 0:
		opts.MaxBreadcrumbs = 0
	case raw.MaxBreadcrumbs > MaxBreadcrumbs:
		opts.MaxBreadcrumbs = MaxBreadcrumbs
		warnings = append(warnings, fmt.Sprintf("MaxBreadcrumbs capped at %d", MaxBreadcrumbs))
	default:
		opts.MaxBreadcrumbs = raw.MaxBreadcrumbs
	}

	for _, p := range raw.IgnoreErrors {
		if p == "" {
			warnings = append(warnings, "IgnoreErrors entries must be non-empty; entry ignored")
			continue
		}
		re, err := regexp.Compile(p)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("IgnoreErrors entry %q is not a valid regular expression; matching it literally", p))
			re = regexp.MustCompile(regexp.QuoteMeta(p))
		}
		opts.IgnoreErrors = append(opts.IgnoreErrors, re)
	}
	return opts, warnings, errs
}

func limitOpt(s string) string {
	s = strings.TrimSpace(s)
	return Truncate(s, 200)
}

func parseEndpoint(v string) (string, bool) {
	u, err := url.Parse(strings.TrimSpace(v))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", false
	}
	return strings.TrimRight(u.String(), "/"), true
}
