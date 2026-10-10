package transport

import (
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/getfaultline/faultline-go/internal/contracttest"
	"github.com/getfaultline/faultline-go/internal/domain"
	"github.com/getfaultline/faultline-go/internal/infrastructure/sysinfo"
)

func event(msg string) *domain.Event {
	return &domain.Event{
		EventID:   sysinfo.NewUUID(),
		Timestamp: time.Now(),
		Level:     domain.LevelError,
		Platform:  domain.Platform,
		Message:   msg,
		SDK:       domain.SDKInfo{Name: domain.SDKName, Version: "0.1.0"},
	}
}

func newTransport(t *testing.T, srv *contracttest.Server, o Options) *HTTP {
	t.Helper()
	o.Endpoint = srv.URL
	o.APIKey = "flt_ingest_test"
	if o.BackoffBase == 0 {
		o.BackoffBase = 10 * time.Millisecond
	}
	if o.FlushInterval == 0 {
		o.FlushInterval = time.Hour // only explicit triggers in tests
	}
	tr := New(o)
	t.Cleanup(func() { tr.Close(time.Second) })
	return tr
}

func TestSendsGzipBatchWithHeaders(t *testing.T) {
	srv := contracttest.NewServer()
	defer srv.Close()
	tr := newTransport(t, srv, Options{UserAgent: "faultline-go/test"})
	tr.Send(event("a"))
	tr.Send(event("b"))
	if !tr.Flush(2 * time.Second) {
		t.Fatal("flush timed out")
	}
	reqs := srv.Requests()
	if len(reqs) != 1 || len(reqs[0].Events) != 2 {
		t.Fatalf("expected one batch of 2, got %d requests", len(reqs))
	}
	r := reqs[0]
	if !r.Gzip || r.Header.Get("X-Faultline-Key") != "flt_ingest_test" || r.Header.Get("Content-Type") != "application/json" || r.Header.Get("User-Agent") != "faultline-go/test" {
		t.Fatalf("headers: %v", r.Header)
	}
	if err := contracttest.ValidateBatch(r.Body); err != nil {
		t.Fatal(err)
	}
}

func TestFlushAtThresholdAndInterval(t *testing.T) {
	srv := contracttest.NewServer()
	defer srv.Close()
	tr := newTransport(t, srv, Options{})
	for i := 0; i < 20; i++ {
		tr.Send(event(fmt.Sprint(i)))
	}
	if !srv.WaitForRequests(1, 2*time.Second) {
		t.Fatal("20 queued events must trigger a send")
	}

	srv2 := contracttest.NewServer()
	defer srv2.Close()
	tr2 := newTransport(t, srv2, Options{FlushInterval: 50 * time.Millisecond})
	tr2.Send(event("x"))
	if !srv2.WaitForRequests(1, 2*time.Second) {
		t.Fatal("interval must trigger a send")
	}
}

func TestBatchesAreCappedAt50(t *testing.T) {
	srv := contracttest.NewServer()
	defer srv.Close()
	tr := newTransport(t, srv, Options{FlushAt: 1000, MaxQueueSize: 1000})
	for i := 0; i < 120; i++ {
		tr.Send(event(fmt.Sprint(i)))
	}
	tr.Flush(2 * time.Second)
	reqs := srv.Requests()
	if len(reqs) != 3 || len(reqs[0].Events) != 50 || len(reqs[2].Events) != 20 {
		t.Fatalf("batches: %d", len(reqs))
	}
}

func TestQueueDropsOldest(t *testing.T) {
	srv := contracttest.NewServer()
	defer srv.Close()
	tr := newTransport(t, srv, Options{FlushAt: 1000})
	for i := 0; i < 150; i++ {
		tr.Send(event(fmt.Sprint(i)))
	}
	if tr.QueueLen() != 100 {
		t.Fatalf("queue %d", tr.QueueLen())
	}
	tr.Flush(2 * time.Second)
	events := srv.Events()
	if len(events) != 100 || events[0]["message"] != "50" {
		t.Fatalf("expected the newest 100 starting at 50, got %d starting %v", len(events), events[0]["message"])
	}
}

func TestRetriesServerErrorsWithBackoff(t *testing.T) {
	srv := contracttest.NewServer(contracttest.Response{Status: 500}, contracttest.Response{Status: 503})
	defer srv.Close()
	tr := newTransport(t, srv, Options{})
	tr.Send(event("retry"))
	if !tr.Flush(3 * time.Second) {
		t.Fatal("flush")
	}
	if n := len(srv.Requests()); n != 3 {
		t.Fatalf("expected 2 failures then success, got %d requests", n)
	}
}

func TestGivesUpAfterMaxRetries(t *testing.T) {
	srv := contracttest.NewServer(
		contracttest.Response{Status: 500}, contracttest.Response{Status: 500},
		contracttest.Response{Status: 500}, contracttest.Response{Status: 500},
	)
	defer srv.Close()
	tr := newTransport(t, srv, Options{})
	tr.Send(event("doomed"))
	tr.Flush(3 * time.Second)
	if n := len(srv.Requests()); n != 4 {
		t.Fatalf("1 attempt + 3 retries expected, got %d", n)
	}
}

func TestDropsOn401And413(t *testing.T) {
	for _, status := range []int{401, 413, 400} {
		srv := contracttest.NewServer(contracttest.Response{Status: status})
		tr := newTransport(t, srv, Options{})
		tr.Send(event("x"))
		if !tr.Flush(2 * time.Second) {
			t.Fatalf("%d: flush", status)
		}
		time.Sleep(30 * time.Millisecond)
		if n := len(srv.Requests()); n != 1 {
			t.Fatalf("%d: must not retry, got %d requests", status, n)
		}
		srv.Close()
	}
}

func TestHonors429RetryAfter(t *testing.T) {
	srv := contracttest.NewServer(contracttest.Response{Status: http.StatusTooManyRequests, RetryAfter: "1"})
	defer srv.Close()
	tr := newTransport(t, srv, Options{})
	tr.Send(event("limited"))
	start := time.Now()
	tr.signal()
	if !srv.WaitForRequests(1, time.Second) {
		t.Fatal("first request")
	}
	if tr.Flush(300 * time.Millisecond) {
		t.Fatal("flush must not succeed while rate limited")
	}
	if !srv.WaitForRequests(2, 3*time.Second) {
		t.Fatal("event must be retried after Retry-After")
	}
	if elapsed := time.Since(start); elapsed < 900*time.Millisecond {
		t.Fatalf("retried too early: %s", elapsed)
	}
	events := srv.Requests()[1].Events
	if len(events) != 1 || events[0]["message"] != "limited" {
		t.Fatalf("requeued event: %v", events)
	}
}

func TestParseRetryAfter(t *testing.T) {
	tr := &HTTP{o: Options{MaxRetryAfter: time.Minute, DefaultRetryAfter: 10 * time.Second}}
	cases := map[string]time.Duration{"": 10 * time.Second, "2": 2 * time.Second, "junk": 10 * time.Second, "-5": 0, "3600": time.Minute}
	for in, want := range cases {
		if got := tr.parseRetryAfter(in); got != want {
			t.Errorf("%q: %s", in, got)
		}
	}
	date := time.Now().Add(5 * time.Second).UTC().Format(http.TimeFormat)
	if got := tr.parseRetryAfter(date); got < 3*time.Second || got > 6*time.Second {
		t.Errorf("http date: %s", got)
	}
}

func TestBackoffJitterBounds(t *testing.T) {
	for _, r := range []float64{0, 0.5, 0.999} {
		tr := &HTTP{o: Options{BackoffBase: time.Second, BackoffMax: 30 * time.Second, Random: func() float64 { return r }}}
		for attempt, exp := range []time.Duration{time.Second, 2 * time.Second, 4 * time.Second} {
			got := tr.Backoff(attempt)
			if got < exp/2 || got > exp {
				t.Errorf("attempt %d r=%v: %s", attempt, r, got)
			}
		}
		if got := tr.Backoff(20); got > 30*time.Second {
			t.Errorf("cap: %s", got)
		}
	}
}

func TestTimeoutIsRetried(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	srv := contracttest.NewServer()
	defer srv.Close()
	slow := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		if n == 1 {
			time.Sleep(300 * time.Millisecond)
		}
		w.WriteHeader(202)
	})
	srv.Config.Handler = slow
	tr := newTransport(t, srv, Options{Timeout: 100 * time.Millisecond})
	tr.Send(event("slow"))
	if !tr.Flush(3 * time.Second) {
		t.Fatal("flush")
	}
	mu.Lock()
	defer mu.Unlock()
	if calls != 2 {
		t.Fatalf("calls %d", calls)
	}
}

func TestCloseStopsAcceptingAndNeverPanics(t *testing.T) {
	srv := contracttest.NewServer()
	defer srv.Close()
	tr := newTransport(t, srv, Options{})
	tr.Send(nil)
	tr.Send(event("before"))
	if !tr.Close(time.Second) {
		t.Fatal("close should flush")
	}
	tr.Send(event("after"))
	tr.Close(time.Second)
	if n := len(srv.Events()); n != 1 {
		t.Fatalf("events %d", n)
	}
}

func TestUnreachableServerDoesNotBlock(t *testing.T) {
	tr := New(Options{Endpoint: "http://127.0.0.1:1", APIKey: "k", BackoffBase: time.Millisecond, FlushInterval: time.Hour})
	start := time.Now()
	tr.Send(event("x"))
	tr.Flush(2 * time.Second)
	tr.Close(100 * time.Millisecond)
	if time.Since(start) > 3*time.Second {
		t.Fatal("too slow")
	}
}
