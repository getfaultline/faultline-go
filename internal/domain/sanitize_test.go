package domain

import (
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestTruncate(t *testing.T) {
	if got := Truncate("hello", 5); got != "hello" {
		t.Fatal(got)
	}
	if got := Truncate("hello world", 5); got != "hell…" {
		t.Fatal(got)
	}
	if got := Truncate("héllo wörld", 6); got != "héllo…" || utf8.RuneCountInString(got) != 6 {
		t.Fatal(got)
	}
	if got := Truncate("x", 0); got != "" {
		t.Fatal(got)
	}
}

func TestSanitizeEnforcesLimits(t *testing.T) {
	e := baseEvent()
	e.Level = "nope"
	e.Platform = "node"
	e.SDK = SDKInfo{Name: "other", Version: "x"}
	e.Message = strings.Repeat("m", 10000)
	e.Tags = map[string]string{}
	for i := 0; i < 60; i++ {
		e.Tags[fmt.Sprintf("tag-%02d-%s", i, strings.Repeat("k", 40))] = strings.Repeat("v", 300)
	}
	var frames []StackFrame
	for i := 0; i < 150; i++ {
		frames = append(frames, StackFrame{Filename: fmt.Sprintf("f%d.go", i), Lineno: i + 1, ContextLine: strings.Repeat("c", 400),
			PreContext: []string{"1", "2", "3", "4", "5", "6", "7"}, PostContext: []string{"a", "b", "c", "d", "e", "f"}})
	}
	ex := &Exception{Type: "root", Stacktrace: Stacktrace{Frames: frames}}
	cur := ex
	for i := 0; i < 8; i++ {
		cur.Cause = &Exception{Type: fmt.Sprintf("cause%d", i)}
		cur = cur.Cause
	}
	e.Exception = ex
	for i := 0; i < 130; i++ {
		e.Breadcrumbs = append(e.Breadcrumbs, Breadcrumb{Message: fmt.Sprint(i), Type: "bogus", Level: "loud"})
	}

	out := SanitizeEvent(e, "1.2.3", time.Now())
	if out.Level != LevelError || out.Platform != "go" || out.SDK.Name != "faultline-go" || out.SDK.Version != "1.2.3" {
		t.Fatalf("enums/sdk: %+v", out)
	}
	if n := utf8.RuneCountInString(out.Message); n != MaxMessageLength || !strings.HasSuffix(out.Message, Ellipsis) {
		t.Fatalf("message length %d", n)
	}
	if len(out.Tags) != MaxTags {
		t.Fatalf("tags: %d", len(out.Tags))
	}
	for k, v := range out.Tags {
		if utf8.RuneCountInString(k) > MaxTagKeyLength || utf8.RuneCountInString(v) > MaxTagValueLength {
			t.Fatalf("tag %q=%q too long", k, v)
		}
	}
	f := out.Exception.Stacktrace.Frames
	if len(f) != MaxFrames || f[0].Filename != "f50.go" || f[99].Filename != "f149.go" {
		t.Fatalf("frames should keep the newest 100 in order: %d %s %s", len(f), f[0].Filename, f[len(f)-1].Filename)
	}
	if len(f[0].PreContext) != 5 || f[0].PreContext[0] != "3" || len(f[0].PostContext) != 5 || f[0].PostContext[4] != "e" {
		t.Fatalf("context lines: %v %v", f[0].PreContext, f[0].PostContext)
	}
	if utf8.RuneCountInString(f[0].ContextLine) != MaxContextLineLen {
		t.Fatal("context line not truncated")
	}
	depth := 0
	for c := out.Exception; c != nil; c = c.Cause {
		depth++
	}
	if depth != MaxExceptionDepth {
		t.Fatalf("depth %d", depth)
	}
	if len(out.Breadcrumbs) != MaxBreadcrumbs || out.Breadcrumbs[0].Message != "30" {
		t.Fatalf("breadcrumbs: %d", len(out.Breadcrumbs))
	}
	if out.Breadcrumbs[0].Type != BreadcrumbDefault || out.Breadcrumbs[0].Level != "" || out.Breadcrumbs[0].Timestamp.IsZero() {
		t.Fatalf("breadcrumb not sanitized: %+v", out.Breadcrumbs[0])
	}
}

func TestSanitizeCapsRequestKeys(t *testing.T) {
	e := baseEvent()
	e.Request = &Request{Query: map[string]string{strings.Repeat("q", 300): "v"}}
	out := SanitizeEvent(e, "1", time.Now())
	for k := range out.Request.Query {
		if len(k) != 256 {
			t.Fatalf("key length %d", len(k))
		}
	}
}

func TestSanitizeRejectsUnusable(t *testing.T) {
	if SanitizeEvent(nil, "1", time.Now()) != nil {
		t.Fatal("nil event")
	}
	if SanitizeEvent(&Event{}, "1", time.Now()) != nil {
		t.Fatal("event without id")
	}
}

type cyclic struct{ Self *cyclic }

func TestNormalizeHandlesHostileValues(t *testing.T) {
	m := map[string]any{}
	m["self"] = m
	c := &cyclic{}
	c.Self = c
	extra := map[string]any{
		"chan":   make(chan int),
		"func":   func() {},
		"nan":    math.NaN(),
		"cycle":  m,
		"cyclic": c,
		"err":    fmt.Errorf("boom"),
		"ok":     map[string]int{"a": 1},
		"long":   strings.Repeat("x", 9000),
		"time":   time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
	}
	out := NormalizeMap(extra)
	if _, err := json.Marshal(out); err != nil {
		t.Fatalf("normalized map must marshal: %v", err)
	}
	for _, k := range []string{"chan", "func", "nan", "cycle", "cyclic"} {
		if s, _ := out[k].(string); !strings.HasPrefix(s, "[unserializable") {
			t.Errorf("%s: %v", k, out[k])
		}
	}
	if out["err"] != "boom" {
		t.Errorf("err: %v", out["err"])
	}
	if got := out["ok"].(map[string]any)["a"]; fmt.Sprint(got) != "1" {
		t.Errorf("ok: %v", got)
	}
	if n := utf8.RuneCountInString(out["long"].(string)); n != MaxMessageLength {
		t.Errorf("long: %d", n)
	}
	if out["time"] != "2026-01-02T03:04:05Z" {
		t.Errorf("time: %v", out["time"])
	}
}

func TestSerializeWithinLimitTrims(t *testing.T) {
	e := baseEvent()
	e.Breadcrumbs = []Breadcrumb{{Type: BreadcrumbLog, Message: strings.Repeat("b", 5000), Timestamp: time.Now()}}
	e.Extra = map[string]any{"big": strings.Repeat("e", 3000)}
	raw, ok := SerializeWithinLimit(e, 4000)
	if !ok || strings.Contains(string(raw), "bbbb") || !strings.Contains(string(raw), "eeee") {
		t.Fatalf("expected breadcrumbs trimmed first: ok=%v len=%d", ok, len(raw))
	}
	raw, ok = SerializeWithinLimit(e, 1000)
	if !ok || strings.Contains(string(raw), "eeee") {
		t.Fatalf("expected extra trimmed: ok=%v", ok)
	}
	if _, ok := SerializeWithinLimit(e, 10); ok {
		t.Fatal("cannot fit in 10 bytes")
	}
}

func TestTimestampFormat(t *testing.T) {
	e := baseEvent()
	e.Timestamp = time.Date(2026, 10, 9, 12, 0, 0, 123456789, time.FixedZone("x", 3600))
	raw, _ := json.Marshal(e)
	if !strings.Contains(string(raw), `"timestamp":"2026-10-09T11:00:00.123Z"`) {
		t.Fatalf("timestamp: %s", raw)
	}
}

func TestDedupeKeyAndIgnore(t *testing.T) {
	a := baseEvent()
	a.Exception = &Exception{Type: "*errors.errorString", Value: "boom", Stacktrace: Stacktrace{Frames: []StackFrame{{Filename: "a.go", Function: "main.f", Lineno: 3}}}}
	b := *a
	b.EventID = "other"
	if DedupeKey(a) != DedupeKey(&b) {
		t.Fatal("same error must share the key")
	}
	b.Fingerprint = []string{"x"}
	if DedupeKey(&b) != "fp:x" {
		t.Fatal(DedupeKey(&b))
	}
	if !MatchesIgnoreErrors(a, []*regexp.Regexp{regexp.MustCompile(`errorString: boom`)}) {
		t.Fatal("type: value should match")
	}
	if MatchesIgnoreErrors(a, []*regexp.Regexp{regexp.MustCompile(`^nope$`)}) {
		t.Fatal("should not match")
	}
	m := baseEvent()
	m.Message = "context canceled while reading"
	if !MatchesIgnoreErrors(m, []*regexp.Regexp{regexp.MustCompile(`context canceled`)}) {
		t.Fatal("message substring should match")
	}
}

func TestResolveOptions(t *testing.T) {
	opts, warnings, errs := ResolveOptions(RawOptions{APIKey: " k ", Endpoint: "http://localhost:4000/", SampleRate: 2, MaxBreadcrumbs: 500, IgnoreErrors: []string{"(bad", "ok"}})
	if len(errs) != 0 || opts.APIKey != "k" || opts.Endpoint != "http://localhost:4000" {
		t.Fatalf("%+v %v", opts, errs)
	}
	if opts.SampleRate != 1 || opts.MaxBreadcrumbs != 100 || len(opts.IgnoreErrors) != 2 || len(warnings) != 3 {
		t.Fatalf("defaults/warnings: %+v %v", opts, warnings)
	}
	if !opts.IgnoreErrors[0].MatchString("x (bad y") {
		t.Fatal("invalid regexp should match literally")
	}
	if !opts.Enabled || opts.Endpoint == DefaultEndpoint {
		t.Fatal("enabled by default")
	}
	_, _, errs = ResolveOptions(RawOptions{})
	if len(errs) != 1 {
		t.Fatalf("missing key must be an error: %v", errs)
	}
	_, _, errs = ResolveOptions(RawOptions{APIKey: "k", Endpoint: "ftp://x"})
	if len(errs) != 1 {
		t.Fatalf("bad endpoint must be an error: %v", errs)
	}
	off := false
	o, _, errs := ResolveOptions(RawOptions{Enabled: &off})
	if len(errs) != 0 || o.Enabled {
		t.Fatalf("disabled without key is fine: %v", errs)
	}
	o, _, _ = ResolveOptions(RawOptions{APIKey: "k", MaxBreadcrumbs: -1, SampleRate: 0.25})
	if o.MaxBreadcrumbs != 0 || o.SampleRate != 0.25 || o.Endpoint != DefaultEndpoint {
		t.Fatalf("%+v", o)
	}
}
