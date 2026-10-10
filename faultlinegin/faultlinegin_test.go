package faultlinegin_test

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
	"github.com/getfaultline/faultline-go/faultlinegin"
	"github.com/getfaultline/faultline-go/internal/contracttest"
	"github.com/gin-gonic/gin"
)

func init() { gin.SetMode(gin.TestMode) }

func setup(t *testing.T) *contracttest.Server {
	t.Helper()
	faultline.Close(time.Second)
	srv := contracttest.NewServer()
	if err := faultline.Init(faultline.Options{APIKey: "flt_ingest_test", Endpoint: srv.URL}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		faultline.Close(time.Second)
		srv.Close()
	})
	return srv
}

func flushed(t *testing.T, srv *contracttest.Server) []map[string]any {
	t.Helper()
	faultline.Flush(3 * time.Second)
	for _, r := range srv.Requests() {
		if err := contracttest.ValidateBatch(r.Body); err != nil {
			t.Fatal(err)
		}
	}
	return srv.Events()
}

func router(opts faultlinegin.Options) *gin.Engine {
	r := gin.New()
	r.Use(faultlinegin.New(opts))
	r.GET("/users/:id/panic", func(c *gin.Context) {
		faultlinegin.GetScope(c).SetUser(faultline.User{ID: c.Param("id")})
		panic(errors.New("failed for " + c.Param("id")))
	})
	r.GET("/error", func(c *gin.Context) {
		_ = c.Error(fmt.Errorf("load order: %w", errors.New("db down")))
		c.AbortWithStatus(http.StatusInternalServerError)
	})
	r.GET("/unavailable", func(c *gin.Context) { c.Status(http.StatusServiceUnavailable) })
	r.GET("/bad", func(c *gin.Context) { c.Status(http.StatusBadRequest) })
	r.GET("/ok", func(c *gin.Context) { c.String(http.StatusOK, "ok") })
	return r
}

func TestRecoversPanics(t *testing.T) {
	srv := setup(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/users/42/panic?token=s", nil)
	req.Header.Set("Cookie", "sid=1")
	router(faultlinegin.Options{}).ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status %d", rec.Code)
	}
	evs := flushed(t, srv)
	if len(evs) != 1 {
		t.Fatalf("events %d", len(evs))
	}
	ev := evs[0]
	ex := ev["exception"].(map[string]any)
	if ev["level"] != "fatal" || ex["value"] != "failed for 42" || ev["user"].(map[string]any)["id"] != "42" {
		t.Fatalf("event: %v", ev)
	}
	if ev["tags"].(map[string]any)["route"] != "/users/:id/panic" {
		t.Fatalf("tags: %v", ev["tags"])
	}
	r := ev["request"].(map[string]any)
	if !strings.HasSuffix(r["url"].(string), "token=[Filtered]") {
		t.Fatalf("url: %v", r["url"])
	}
	if _, ok := r["headers"].(map[string]any)["Cookie"]; ok {
		t.Fatal("cookie header must be removed without SendDefaultPII")
	}
	frames := ex["stacktrace"].(map[string]any)["frames"].([]any)
	if top := frames[len(frames)-1].(map[string]any); top["function"] != "faultlinegin_test.router.func1" {
		t.Fatalf("newest frame: %v", top)
	}
}

func TestCapturesContextErrorsAnd5xx(t *testing.T) {
	srv := setup(t)
	r := router(faultlinegin.Options{})
	for _, p := range []string{"/ok", "/bad", "/error", "/unavailable"} {
		r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", p, nil))
	}
	evs := flushed(t, srv)
	if len(evs) != 2 {
		t.Fatalf("events %d", len(evs))
	}
	chain := evs[0]["exception"].(map[string]any)
	if chain["type"] != "*fmt.wrapError" || chain["cause"].(map[string]any)["value"] != "db down" {
		t.Fatalf("c.Error capture: %v", chain)
	}
	if evs[1]["message"] != "HTTP 503 Service Unavailable: GET /unavailable" {
		t.Fatalf("5xx message: %v", evs[1]["message"])
	}
}

func TestRepanicWorksWithGinRecovery(t *testing.T) {
	srv := setup(t)
	r := gin.New()
	r.Use(gin.CustomRecovery(func(c *gin.Context, _ any) { c.AbortWithStatus(599) }))
	r.Use(faultlinegin.New(faultlinegin.Options{Repanic: true}))
	r.GET("/p", func(c *gin.Context) { panic("boom") })
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest("GET", "/p", nil))
	if rec.Code != 599 {
		t.Fatalf("gin.Recovery should have handled the re-panic, got %d", rec.Code)
	}
	if evs := flushed(t, srv); len(evs) != 1 || evs[0]["exception"].(map[string]any)["type"] != "panic" {
		t.Fatalf("events: %v", evs)
	}
}

func TestConcurrentRequestsAreIsolated(t *testing.T) {
	srv := setup(t)
	ts := httptest.NewServer(router(faultlinegin.Options{}))
	defer ts.Close()
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if resp, err := http.Get(fmt.Sprintf("%s/users/%d/panic", ts.URL, i)); err == nil {
				resp.Body.Close()
			}
		}(i)
	}
	wg.Wait()
	evs := flushed(t, srv)
	if len(evs) != 20 {
		t.Fatalf("events %d", len(evs))
	}
	for _, ev := range evs {
		id := ev["user"].(map[string]any)["id"].(string)
		if ev["message"] != "failed for "+id || len(ev["breadcrumbs"].([]any)) != 1 {
			t.Fatalf("scope leaked: %v", ev)
		}
	}
}

func TestGetScopeWithoutMiddleware(t *testing.T) {
	if faultlinegin.GetScope(nil) != nil {
		t.Fatal("nil context")
	}
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("GET", "/", nil)
	faultlinegin.GetScope(c).SetTag("a", "b") // nil scope: no-op, no panic
}
