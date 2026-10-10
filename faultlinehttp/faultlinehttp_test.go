package faultlinehttp_test

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	faultline "github.com/getfaultline/faultline-go"
	"github.com/getfaultline/faultline-go/faultlinehttp"
	"github.com/getfaultline/faultline-go/internal/contracttest"
)

func setup(t *testing.T, opts faultline.Options) *contracttest.Server {
	t.Helper()
	faultline.Close(time.Second)
	srv := contracttest.NewServer()
	opts.APIKey, opts.Endpoint = "flt_ingest_test", srv.URL
	if err := faultline.Init(opts); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		faultline.Close(time.Second)
		srv.Close()
	})
	return srv
}

func events(t *testing.T, srv *contracttest.Server) []map[string]any {
	t.Helper()
	faultline.Flush(3 * time.Second)
	for _, r := range srv.Requests() {
		if err := contracttest.ValidateBatch(r.Body); err != nil {
			t.Fatal(err)
		}
	}
	return srv.Events()
}

func newMux() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/ok", func(w http.ResponseWriter, r *http.Request) {
		faultline.ScopeFromContext(r.Context()).SetTag("route", "ok")
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/panic", func(w http.ResponseWriter, r *http.Request) {
		faultline.ScopeFromContext(r.Context()).SetUser(faultline.User{ID: r.URL.Query().Get("user")})
		panic(errors.New("handler exploded"))
	})
	mux.HandleFunc("/user", func(w http.ResponseWriter, r *http.Request) {
		user := r.URL.Query().Get("user")
		faultline.ScopeFromContext(r.Context()).SetUser(faultline.User{ID: user})
		faultline.ScopeFromContext(r.Context()).AddBreadcrumb(faultline.Breadcrumb{Message: "work " + user})
		panic(errors.New("failed for " + user))
	})
	mux.HandleFunc("/fail", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusBadGateway)
	})
	mux.HandleFunc("/notfound", func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})
	mux.HandleFunc("/abort", func(w http.ResponseWriter, r *http.Request) {
		panic(http.ErrAbortHandler)
	})
	return mux
}

func TestRecoversPanicsWithRequestData(t *testing.T) {
	srv := setup(t, faultline.Options{})
	h := faultlinehttp.New(faultlinehttp.Options{}).Handle(newMux())
	req := httptest.NewRequest("GET", "http://api.test/panic?user=u7&token=abc", nil)
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("Accept", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status %d", rec.Code)
	}
	evs := events(t, srv)
	if len(evs) != 1 {
		t.Fatalf("events %d", len(evs))
	}
	ev := evs[0]
	ex := ev["exception"].(map[string]any)
	if ev["level"] != "fatal" || ex["type"] != "*errors.errorString" || ex["value"] != "handler exploded" {
		t.Fatalf("event: %v", ev)
	}
	req2 := ev["request"].(map[string]any)
	if req2["method"] != "GET" || req2["url"] != "http://api.test/panic?user=u7&token=[Filtered]" {
		t.Fatalf("request: %v", req2)
	}
	headers := req2["headers"].(map[string]any)
	if _, ok := headers["Authorization"]; ok || headers["Accept"] != "application/json" {
		t.Fatalf("headers: %v", headers)
	}
	if req2["query"].(map[string]any)["token"] != "[Filtered]" || ev["user"].(map[string]any)["id"] != "u7" {
		t.Fatalf("query/user: %v", ev)
	}
	crumbs := ev["breadcrumbs"].([]any)
	if c := crumbs[0].(map[string]any); c["type"] != "http" || c["message"] != "GET /panic" {
		t.Fatalf("breadcrumb: %v", c)
	}
	frames := ex["stacktrace"].(map[string]any)["frames"].([]any)
	top := frames[len(frames)-1].(map[string]any)
	if top["function"] != "faultlinehttp_test.newMux.func2" {
		t.Fatalf("newest frame should be the handler: %v", top)
	}
}

func TestReports5xxButNot4xxOr2xx(t *testing.T) {
	srv := setup(t, faultline.Options{})
	h := faultlinehttp.Middleware(newMux())
	for _, path := range []string{"/ok", "/notfound", "/fail"} {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", path, nil))
	}
	evs := events(t, srv)
	if len(evs) != 1 || evs[0]["message"] != "HTTP 502 Bad Gateway: GET /fail" || evs[0]["level"] != "error" {
		t.Fatalf("events: %v", evs)
	}
	if evs[0]["tags"].(map[string]any)["http.status_code"] != "502" {
		t.Fatalf("tags: %v", evs[0]["tags"])
	}
}

func TestCustomCaptureStatusAndRepanic(t *testing.T) {
	srv := setup(t, faultline.Options{})
	h := faultlinehttp.New(faultlinehttp.Options{
		Repanic:       true,
		CaptureStatus: func(s int) bool { return s == http.StatusNotFound },
	}).Handle(newMux())
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/notfound", nil))
	func() {
		defer func() {
			if recover() == nil {
				t.Error("Repanic must re-panic")
			}
		}()
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/panic", nil))
	}()
	if n := len(events(t, srv)); n != 2 {
		t.Fatalf("events %d", n)
	}
}

func TestAbortHandlerIsNotCaptured(t *testing.T) {
	srv := setup(t, faultline.Options{})
	h := faultlinehttp.Middleware(newMux())
	func() {
		defer func() {
			if v := recover(); v != http.ErrAbortHandler {
				t.Errorf("ErrAbortHandler must propagate, got %v", v)
			}
		}()
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/abort", nil))
	}()
	if n := len(events(t, srv)); n != 0 {
		t.Fatalf("events %d", n)
	}
}

func TestConcurrentRequestsAreIsolated(t *testing.T) {
	srv := setup(t, faultline.Options{})
	ts := httptest.NewServer(faultlinehttp.Middleware(newMux()))
	defer ts.Close()
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			resp, err := http.Get(fmt.Sprintf("%s/user?user=u%d", ts.URL, i))
			if err == nil {
				resp.Body.Close()
			}
		}(i)
	}
	wg.Wait()
	evs := events(t, srv)
	if len(evs) != 20 {
		t.Fatalf("events %d", len(evs))
	}
	for _, ev := range evs {
		user := ev["user"].(map[string]any)["id"].(string)
		url := ev["request"].(map[string]any)["url"].(string)
		if !strings.HasSuffix(url, "user="+user) || ev["message"] != "failed for "+user {
			t.Fatalf("request %s got user %s (%v)", url, user, ev["message"])
		}
		crumbs := ev["breadcrumbs"].([]any)
		if len(crumbs) != 2 || crumbs[1].(map[string]any)["message"] != "work "+user {
			t.Fatalf("breadcrumbs leaked across requests: %v", crumbs)
		}
	}
}

func TestSendDefaultPIIRecordsIP(t *testing.T) {
	srv := setup(t, faultline.Options{SendDefaultPII: true})
	h := faultlinehttp.Middleware(newMux())
	req := httptest.NewRequest("GET", "/fail", nil)
	req.RemoteAddr = "203.0.113.9:5555"
	req.Header.Set("Cookie", "sid=1")
	h.ServeHTTP(httptest.NewRecorder(), req)
	ev := events(t, srv)[0]
	if ev["user"].(map[string]any)["ipAddress"] != "203.0.113.9" {
		t.Fatalf("user: %v", ev["user"])
	}
	if ev["request"].(map[string]any)["headers"].(map[string]any)["Cookie"] != "[Filtered]" {
		t.Fatal("cookie value must still be filtered")
	}
}

func TestFlusherPassthrough(t *testing.T) {
	setup(t, faultline.Options{})
	h := faultlinehttp.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := http.NewResponseController(w).Flush(); err != nil {
			t.Errorf("flush via ResponseController: %v", err)
		}
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if !rec.Flushed {
		t.Fatal("not flushed")
	}
}
