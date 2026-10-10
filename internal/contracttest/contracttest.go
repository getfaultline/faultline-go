// Package contracttest is test-only support code: a JSON Schema mirroring the event contract
// (CONTRACTS.md sections 2 and 7), a minimal validator for the keywords that schema uses, and
// a mock ingest server. Only _test.go files import it; it is never linked into the SDK.
package contracttest

import (
	"bytes"
	"compress/gzip"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

//go:embed event.schema.json
var schemaJSON []byte

var (
	schemaOnce sync.Once
	schemaRoot map[string]any
	schemaErr  error
	dateTimeRe = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?Z$`)
)

// Schema returns the parsed schema.
func Schema() (map[string]any, error) {
	schemaOnce.Do(func() {
		schemaErr = json.Unmarshal(schemaJSON, &schemaRoot)
	})
	return schemaRoot, schemaErr
}

func decode(raw []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	return v, nil
}

// ValidateBatch validates a request body ({"events": [...]}) against the schema.
func ValidateBatch(raw []byte) error {
	v, err := decode(raw)
	if err != nil {
		return err
	}
	root, err := Schema()
	if err != nil {
		return err
	}
	var errs []string
	validate(root, root, v, "$", &errs)
	if m, ok := v.(map[string]any); ok {
		if events, ok := m["events"].([]any); ok {
			for i, e := range events {
				checkDepth(e, fmt.Sprintf("$.events[%d]", i), &errs)
			}
		}
	}
	return joinErrs(errs)
}

// ValidateEvent validates one serialized event against #/$defs/event.
func ValidateEvent(raw []byte) error {
	v, err := decode(raw)
	if err != nil {
		return err
	}
	root, err := Schema()
	if err != nil {
		return err
	}
	var errs []string
	validate(root, resolveRef(root, "#/$defs/event"), v, "$", &errs)
	checkDepth(v, "$", &errs)
	return joinErrs(errs)
}

func joinErrs(errs []string) error {
	if len(errs) == 0 {
		return nil
	}
	return fmt.Errorf("payload does not conform to the contract:\n  %s", strings.Join(errs, "\n  "))
}

// checkDepth enforces "exception.cause depth <= 5" (5 exceptions in total), which JSON Schema
// cannot express without unrolling.
func checkDepth(event any, path string, errs *[]string) {
	m, _ := event.(map[string]any)
	depth := 0
	for ex, _ := m["exception"].(map[string]any); ex != nil; ex, _ = ex["cause"].(map[string]any) {
		depth++
	}
	if depth > 5 {
		*errs = append(*errs, fmt.Sprintf("%s.exception: cause chain has %d exceptions (max 5)", path, depth))
	}
}

func resolveRef(root map[string]any, ref string) map[string]any {
	cur := any(root)
	for _, part := range strings.Split(strings.TrimPrefix(ref, "#/"), "/") {
		m, _ := cur.(map[string]any)
		cur = m[part]
	}
	s, _ := cur.(map[string]any)
	return s
}

func typeOf(v any) string {
	switch t := v.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case string:
		return "string"
	case json.Number:
		if strings.ContainsAny(string(t), ".eE") {
			return "number"
		}
		return "integer"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	}
	return fmt.Sprintf("%T", v)
}

func typeMatches(want, got string) bool {
	return want == got || (want == "number" && got == "integer")
}

func num(v any) (float64, bool) {
	n, ok := v.(json.Number)
	if !ok {
		f, ok := v.(float64)
		return f, ok
	}
	f, err := n.Float64()
	return f, err == nil
}

// validate implements the JSON Schema keywords used by event.schema.json: $ref, type, enum,
// const, required, properties, additionalProperties, propertyNames, maxProperties, items,
// minItems, maxItems, minLength, maxLength, pattern, format (date-time) and minimum.
func validate(root, schema map[string]any, v any, path string, errs *[]string) {
	if schema == nil {
		return
	}
	fail := func(format string, args ...any) {
		*errs = append(*errs, path+": "+fmt.Sprintf(format, args...))
	}
	if ref, ok := schema["$ref"].(string); ok {
		validate(root, resolveRef(root, ref), v, path, errs)
	}
	got := typeOf(v)
	switch t := schema["type"].(type) {
	case string:
		if !typeMatches(t, got) {
			fail("expected %s, got %s", t, got)
			return
		}
	case []any:
		ok := false
		for _, w := range t {
			if s, _ := w.(string); typeMatches(s, got) {
				ok = true
			}
		}
		if !ok {
			fail("expected one of %v, got %s", t, got)
			return
		}
	}
	if enum, ok := schema["enum"].([]any); ok {
		found := false
		for _, e := range enum {
			if fmt.Sprint(e) == fmt.Sprint(v) && typeOf(e) == got {
				found = true
			}
		}
		if !found {
			fail("%v is not one of %v", v, enum)
		}
	}
	if c, ok := schema["const"]; ok && (fmt.Sprint(c) != fmt.Sprint(v) || typeOf(c) != got) {
		fail("expected %v, got %v", c, v)
	}
	switch t := v.(type) {
	case string:
		n := utf8.RuneCountInString(t)
		if m, ok := num(schema["maxLength"]); ok && float64(n) > m {
			fail("length %d exceeds maxLength %v", n, m)
		}
		if m, ok := num(schema["minLength"]); ok && float64(n) < m {
			fail("length %d below minLength %v", n, m)
		}
		if p, ok := schema["pattern"].(string); ok && !regexp.MustCompile(p).MatchString(t) {
			fail("%q does not match %s", t, p)
		}
		if f, _ := schema["format"].(string); f == "date-time" && !dateTimeRe.MatchString(t) {
			fail("%q is not an ISO 8601 UTC date-time", t)
		}
	case json.Number:
		if m, ok := num(schema["minimum"]); ok {
			if f, _ := t.Float64(); f < m {
				fail("%v below minimum %v", t, m)
			}
		}
	case []any:
		if m, ok := num(schema["maxItems"]); ok && float64(len(t)) > m {
			fail("%d items exceed maxItems %v", len(t), m)
		}
		if m, ok := num(schema["minItems"]); ok && float64(len(t)) < m {
			fail("%d items below minItems %v", len(t), m)
		}
		if items, ok := schema["items"].(map[string]any); ok {
			for i, item := range t {
				validate(root, items, item, fmt.Sprintf("%s[%d]", path, i), errs)
			}
		}
	case map[string]any:
		if req, ok := schema["required"].([]any); ok {
			for _, r := range req {
				if _, ok := t[r.(string)]; !ok {
					fail("missing required property %q", r)
				}
			}
		}
		if m, ok := num(schema["maxProperties"]); ok && float64(len(t)) > m {
			fail("%d properties exceed maxProperties %v", len(t), m)
		}
		props, _ := schema["properties"].(map[string]any)
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			child := path + "." + k
			if pn, ok := schema["propertyNames"].(map[string]any); ok {
				validate(root, pn, k, child+" (name)", errs)
			}
			if ps, ok := props[k].(map[string]any); ok {
				validate(root, ps, t[k], child, errs)
				continue
			}
			switch ap := schema["additionalProperties"].(type) {
			case bool:
				if !ap {
					fail("unexpected property %q", k)
				}
			case map[string]any:
				validate(root, ap, t[k], child, errs)
			}
		}
	}
}

// Request is one request received by Server.
type Request struct {
	Header http.Header
	Body   []byte // decompressed
	Gzip   bool
	Events []map[string]any
}

// Server is a mock ingest API. Responses are popped from Responses (default 202).
type Server struct {
	*httptest.Server

	mu        sync.Mutex
	requests  []Request
	responses []Response
	notify    chan struct{}
}

// Response is a canned reply.
type Response struct {
	Status     int
	RetryAfter string
	Body       string
}

// NewServer starts a mock ingest server. Close it with s.Close().
func NewServer(responses ...Response) *Server {
	s := &Server{responses: responses, notify: make(chan struct{}, 1024)}
	s.Server = httptest.NewServer(http.HandlerFunc(s.handle))
	return s
}

// Enqueue appends canned responses.
func (s *Server) Enqueue(r ...Response) {
	s.mu.Lock()
	s.responses = append(s.responses, r...)
	s.mu.Unlock()
}

func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	isGzip := r.Header.Get("Content-Encoding") == "gzip"
	body := raw
	if isGzip {
		if zr, err := gzip.NewReader(bytes.NewReader(raw)); err == nil {
			body, _ = io.ReadAll(zr)
		}
	}
	var batch struct {
		Events []map[string]any `json:"events"`
	}
	_ = json.Unmarshal(body, &batch)

	s.mu.Lock()
	s.requests = append(s.requests, Request{Header: r.Header.Clone(), Body: body, Gzip: isGzip, Events: batch.Events})
	resp := Response{Status: http.StatusAccepted}
	if len(s.responses) > 0 {
		resp = s.responses[0]
		s.responses = s.responses[1:]
	}
	s.mu.Unlock()
	select {
	case s.notify <- struct{}{}:
	default:
	}

	if resp.RetryAfter != "" {
		w.Header().Set("Retry-After", resp.RetryAfter)
	}
	if resp.Body == "" && resp.Status == http.StatusAccepted {
		resp.Body = fmt.Sprintf(`{"accepted":%d,"rejected":[]}`, len(batch.Events))
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.Status)
	_, _ = io.WriteString(w, resp.Body)
}

// Requests returns a copy of the requests received so far.
func (s *Server) Requests() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Request(nil), s.requests...)
}

// Events returns all events received so far, in order.
func (s *Server) Events() []map[string]any {
	var out []map[string]any
	for _, r := range s.Requests() {
		out = append(out, r.Events...)
	}
	return out
}

// WaitForRequests blocks until n requests arrived or timeout passes.
func (s *Server) WaitForRequests(n int, timeout time.Duration) bool {
	deadline := time.After(timeout)
	for {
		if len(s.Requests()) >= n {
			return true
		}
		select {
		case <-s.notify:
		case <-deadline:
			return len(s.Requests()) >= n
		}
	}
}
