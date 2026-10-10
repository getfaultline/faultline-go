// Package transport delivers events to the Faultline ingest API over HTTP.
package transport

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"math/rand"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/getfaultline/faultline-go/internal/application"
	"github.com/getfaultline/faultline-go/internal/domain"
)

// Options configures the HTTP transport. Zero values use the defaults in parentheses.
type Options struct {
	Endpoint          string
	APIKey            string
	Logger            application.Logger
	HTTPClient        *http.Client
	UserAgent         string
	FlushInterval     time.Duration // 2s
	FlushAt           int           // 20 queued events trigger a send
	MaxQueueSize      int           // 100, oldest dropped on overflow
	MaxBatchSize      int           // 50 (capped at the contract limit)
	MaxBodyBytes      int           // 1 MB uncompressed (capped at the contract limit)
	Timeout           time.Duration // 5s per request
	MaxRetries        int           // 3 (negative disables retries)
	BackoffBase       time.Duration // 1s
	BackoffMax        time.Duration // 30s
	MaxRetryAfter     time.Duration // 10m cap for 429 Retry-After
	DefaultRetryAfter time.Duration // 10s when a 429 has no usable Retry-After
	DisableGzip       bool
	Random            func() float64
}

type item struct {
	id          string
	json        []byte
	rateLimited int
}

// HTTP is a batched transport: in-memory queue (drop oldest), one background goroutine that
// sends every FlushInterval or when FlushAt events are queued, batches of at most 50 events /
// 1 MB, gzip, per-request timeout, exponential backoff with jitter for network errors, 5xx and
// 408, honors 429 Retry-After, and drops batches on 401, 413 and other 4xx. No method panics.
type HTTP struct {
	o      Options
	url    string
	client *http.Client

	mu               sync.Mutex
	queue            []item
	inflight         bool
	rateLimitedUntil time.Time
	wakeTimer        *time.Timer
	closed           bool

	wake      chan struct{}
	done      chan struct{}
	exited    chan struct{}
	closeOnce sync.Once
}

// New starts the transport's background goroutine.
func New(o Options) *HTTP {
	if o.Logger == nil {
		o.Logger = application.NopLogger{}
	}
	if o.FlushInterval <= 0 {
		o.FlushInterval = 2 * time.Second
	}
	if o.FlushAt <= 0 {
		o.FlushAt = 20
	}
	if o.MaxQueueSize <= 0 {
		o.MaxQueueSize = 100
	}
	if o.MaxBatchSize <= 0 || o.MaxBatchSize > domain.MaxBatchEvents {
		o.MaxBatchSize = domain.MaxBatchEvents
	}
	if o.MaxBodyBytes <= 0 || o.MaxBodyBytes > domain.MaxBodyBytes {
		o.MaxBodyBytes = domain.MaxBodyBytes
	}
	if o.Timeout <= 0 {
		o.Timeout = 5 * time.Second
	}
	if o.MaxRetries == 0 {
		o.MaxRetries = 3
	} else if o.MaxRetries < 0 {
		o.MaxRetries = 0
	}
	if o.BackoffBase <= 0 {
		o.BackoffBase = time.Second
	}
	if o.BackoffMax <= 0 {
		o.BackoffMax = 30 * time.Second
	}
	if o.MaxRetryAfter <= 0 {
		o.MaxRetryAfter = 10 * time.Minute
	}
	if o.DefaultRetryAfter <= 0 {
		o.DefaultRetryAfter = 10 * time.Second
	}
	if o.Random == nil {
		o.Random = rand.Float64 //nolint:gosec // jitter only
	}
	client := o.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: o.Timeout}
	}
	t := &HTTP{
		o:      o,
		url:    strings.TrimRight(o.Endpoint, "/") + domain.IngestPath,
		client: client,
		wake:   make(chan struct{}, 1),
		done:   make(chan struct{}),
		exited: make(chan struct{}),
	}
	go t.run()
	return t
}

// URL is the ingest URL.
func (t *HTTP) URL() string { return t.url }

// QueueLen is the number of events waiting to be sent (for tests).
func (t *HTTP) QueueLen() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.queue)
}

// Send serializes e (trimming it to fit the body limit) and enqueues it.
func (t *HTTP) Send(e *domain.Event) {
	defer t.recoverf("Send")
	if e == nil {
		return
	}
	raw, ok := domain.SerializeWithinLimit(e, t.o.MaxBodyBytes-domain.EnvelopeBytes())
	if !ok {
		t.o.Logger.Warnf("event %s exceeds the %d byte body limit; dropped", e.EventID, t.o.MaxBodyBytes)
		return
	}
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return
	}
	t.queue = append(t.queue, item{id: e.EventID, json: raw})
	for len(t.queue) > t.o.MaxQueueSize {
		t.o.Logger.Debugf("queue full; dropped oldest event %s", t.queue[0].id)
		t.queue = t.queue[1:]
	}
	n := len(t.queue)
	t.mu.Unlock()
	if n >= t.o.FlushAt {
		t.signal()
	}
}

// Flush waits until the queue is empty and nothing is in flight, or the timeout passes.
func (t *HTTP) Flush(timeout time.Duration) (ok bool) {
	defer func() {
		if r := recover(); r != nil {
			t.o.Logger.Errorf("Flush failed: %v", r)
			ok = false
		}
	}()
	deadline := time.Now().Add(timeout)
	for {
		t.mu.Lock()
		idle := len(t.queue) == 0 && !t.inflight
		closed := t.closed
		limitedUntil := t.rateLimitedUntil
		inflight := t.inflight
		t.mu.Unlock()
		if idle {
			return true
		}
		if closed {
			return false
		}
		now := time.Now()
		if !now.Before(deadline) {
			return false
		}
		if !inflight && limitedUntil.After(deadline) {
			return false // rate limited past the deadline: nothing can be sent in time
		}
		t.signal()
		wait := 5 * time.Millisecond
		if rem := deadline.Sub(now); rem < wait {
			wait = rem
		}
		time.Sleep(wait)
	}
}

// Close flushes for up to timeout, then stops the background goroutine and drops whatever
// is still queued.
func (t *HTTP) Close(timeout time.Duration) bool {
	start := time.Now()
	ok := t.Flush(timeout)
	t.mu.Lock()
	t.closed = true
	t.queue = nil
	if t.wakeTimer != nil {
		t.wakeTimer.Stop()
	}
	t.mu.Unlock()
	t.closeOnce.Do(func() { close(t.done) })
	if rem := timeout - time.Since(start); rem > 0 {
		select {
		case <-t.exited:
		case <-time.After(rem):
		}
	}
	return ok
}

// ---------------------------------------------------------------------------------------

func (t *HTTP) recoverf(op string) {
	if r := recover(); r != nil {
		t.o.Logger.Errorf("transport %s failed: %v", op, r)
	}
}

func (t *HTTP) signal() {
	select {
	case t.wake <- struct{}{}:
	default:
	}
}

func (t *HTTP) run() {
	defer close(t.exited)
	ticker := time.NewTicker(t.o.FlushInterval)
	defer ticker.Stop()
	for {
		select {
		case <-t.done:
			return
		case <-ticker.C:
		case <-t.wake:
		}
		t.drain()
	}
}

func (t *HTTP) drain() {
	defer t.recoverf("drain")
	for {
		select {
		case <-t.done:
			return
		default:
		}
		t.mu.Lock()
		if len(t.queue) == 0 {
			t.mu.Unlock()
			return
		}
		if wait := time.Until(t.rateLimitedUntil); wait > 0 {
			if t.wakeTimer != nil {
				t.wakeTimer.Stop()
			}
			t.wakeTimer = time.AfterFunc(wait, t.signal)
			t.mu.Unlock()
			return
		}
		batch := t.takeBatchLocked()
		t.inflight = len(batch) > 0
		t.mu.Unlock()
		if len(batch) == 0 {
			return
		}
		t.sendBatch(batch)
		t.mu.Lock()
		t.inflight = false
		t.mu.Unlock()
	}
}

func (t *HTTP) takeBatchLocked() []item {
	var batch []item
	size := domain.EnvelopeBytes()
	for len(t.queue) > 0 && len(batch) < t.o.MaxBatchSize {
		next := t.queue[0]
		add := len(next.json)
		if len(batch) > 0 {
			add++ // comma
		}
		if size+add > t.o.MaxBodyBytes && len(batch) > 0 {
			break
		}
		size += add
		batch = append(batch, next)
		t.queue = t.queue[1:]
	}
	return batch
}

type resultKind int

const (
	resultOK resultKind = iota
	resultDrop
	resultRateLimited
	resultRetry
)

type postResult struct {
	kind       resultKind
	status     int
	retryAfter time.Duration
	reason     string
	body       []byte
}

func (t *HTTP) sendBatch(batch []item) {
	var body bytes.Buffer
	body.WriteString(`{"events":[`)
	for i, it := range batch {
		if i > 0 {
			body.WriteByte(',')
		}
		body.Write(it.json)
	}
	body.WriteString(`]}`)
	payload := body.Bytes()

	for attempt := 0; ; attempt++ {
		res := t.post(payload)
		switch res.kind {
		case resultOK:
			var parsed struct {
				Rejected []json.RawMessage `json:"rejected"`
			}
			if json.Unmarshal(res.body, &parsed) == nil && len(parsed.Rejected) > 0 {
				t.o.Logger.Debugf("server rejected %d event(s): %s", len(parsed.Rejected), res.body)
			}
			return
		case resultDrop:
			reason := ""
			switch res.status {
			case http.StatusUnauthorized:
				reason = " (invalid or revoked API key)"
			case http.StatusRequestEntityTooLarge:
				reason = " (payload too large)"
			}
			t.o.Logger.Debugf("ingest responded %d%s; dropped %d event(s)", res.status, reason, len(batch))
			return
		case resultRateLimited:
			t.mu.Lock()
			t.rateLimitedUntil = time.Now().Add(res.retryAfter)
			keep := make([]item, 0, len(batch))
			for _, it := range batch {
				it.rateLimited++
				if it.rateLimited <= t.o.MaxRetries {
					keep = append(keep, it)
				}
			}
			if dropped := len(batch) - len(keep); dropped > 0 {
				t.o.Logger.Debugf("dropped %d event(s) after repeated 429s", dropped)
			}
			if !t.closed {
				t.queue = append(keep, t.queue...)
				for len(t.queue) > t.o.MaxQueueSize {
					t.queue = t.queue[1:]
				}
			}
			t.mu.Unlock()
			t.o.Logger.Debugf("rate limited; retrying after %s", res.retryAfter)
			return
		case resultRetry:
			if attempt >= t.o.MaxRetries {
				t.o.Logger.Debugf("giving up after %d attempt(s) (%s); dropped %d event(s)", attempt+1, res.reason, len(batch))
				return
			}
			delay := t.Backoff(attempt)
			t.o.Logger.Debugf("send failed (%s); retry #%d in %s", res.reason, attempt+1, delay)
			timer := time.NewTimer(delay)
			select {
			case <-timer.C:
			case <-t.done:
				timer.Stop()
				return
			}
		}
	}
}

// Backoff is exponential backoff with "equal jitter": half fixed, half random.
func (t *HTTP) Backoff(attempt int) time.Duration {
	exp := float64(t.o.BackoffBase) * math.Pow(2, float64(attempt))
	if exp > float64(t.o.BackoffMax) {
		exp = float64(t.o.BackoffMax)
	}
	return time.Duration(exp/2 + t.o.Random()*exp/2)
}

func (t *HTTP) post(payload []byte) (res postResult) {
	defer func() {
		if r := recover(); r != nil {
			res = postResult{kind: resultRetry, reason: fmt.Sprintf("panic: %v", r)}
		}
	}()
	var reader io.Reader = bytes.NewReader(payload)
	gz := !t.o.DisableGzip
	if gz {
		var buf bytes.Buffer
		w := gzip.NewWriter(&buf)
		if _, err := w.Write(payload); err == nil && w.Close() == nil {
			reader = &buf
		} else {
			gz = false
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), t.o.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.url, reader)
	if err != nil {
		return postResult{kind: resultDrop, reason: err.Error()}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Faultline-Key", t.o.APIKey)
	if gz {
		req.Header.Set("Content-Encoding", "gzip")
	}
	if t.o.UserAgent != "" {
		req.Header.Set("User-Agent", t.o.UserAgent)
	}
	resp, err := t.client.Do(req)
	if err != nil {
		reason := err.Error()
		if ctx.Err() != nil {
			reason = "timeout"
		}
		return postResult{kind: resultRetry, reason: reason}
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	status := resp.StatusCode
	switch {
	case status == http.StatusTooManyRequests:
		return postResult{kind: resultRateLimited, status: status, retryAfter: t.parseRetryAfter(resp.Header.Get("Retry-After"))}
	case status >= 200 && status < 300:
		return postResult{kind: resultOK, status: status, body: body}
	case status >= 500 || status == http.StatusRequestTimeout:
		return postResult{kind: resultRetry, status: status, reason: "HTTP " + strconv.Itoa(status)}
	default:
		return postResult{kind: resultDrop, status: status}
	}
}

// ParseRetryAfter converts a Retry-After header (seconds or HTTP date) to a duration, capped
// at MaxRetryAfter; a missing or invalid header yields DefaultRetryAfter.
func (t *HTTP) parseRetryAfter(h string) time.Duration {
	h = strings.TrimSpace(h)
	d := t.o.DefaultRetryAfter
	if h != "" {
		if secs, err := strconv.ParseFloat(h, 64); err == nil && !math.IsNaN(secs) && !math.IsInf(secs, 0) {
			d = time.Duration(secs * float64(time.Second))
		} else if at, err := http.ParseTime(h); err == nil {
			d = time.Until(at)
		}
	}
	if d < 0 {
		d = 0
	}
	if d > t.o.MaxRetryAfter {
		d = t.o.MaxRetryAfter
	}
	return d
}
