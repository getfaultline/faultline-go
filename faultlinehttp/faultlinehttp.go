// Package faultlinehttp is net/http middleware for Faultline.
//
// It gives every request its own scope (carried by the request context), attaches request
// data and an "http" breadcrumb, recovers and captures panics, and reports responses with a
// 5xx status:
//
//	mux := http.NewServeMux()
//	mux.HandleFunc("/", handler)
//	http.ListenAndServe(":8080", faultlinehttp.New(faultlinehttp.Options{}).Handle(mux))
//
// Inside handlers, use faultline.ScopeFromContext(r.Context()) to tag the request and
// faultline.CaptureExceptionContext(r.Context(), err) to capture with its scope.
package faultlinehttp

import (
	"bufio"
	"fmt"
	"net"
	"net/http"

	faultline "github.com/getfaultline/faultline-go"
)

// Options configures the middleware.
type Options struct {
	// Repanic re-panics after capturing a panic so outer middleware (or net/http itself) can
	// handle it. By default the middleware writes a 500 response if nothing was written yet.
	Repanic bool
	// CaptureStatus decides which response statuses are reported as events when the handler
	// returns normally. Default: status >= 500.
	CaptureStatus func(status int) bool
	// DisableBreadcrumb skips the "http" breadcrumb for the incoming request.
	DisableBreadcrumb bool
}

// Handler is the middleware.
type Handler struct {
	opts Options
}

// New returns the middleware.
func New(opts Options) *Handler { return &Handler{opts: opts} }

// Middleware wraps next with default options.
func Middleware(next http.Handler) http.Handler { return New(Options{}).Handle(next) }

// HandleFunc wraps a handler function.
func (h *Handler) HandleFunc(next http.HandlerFunc) http.HandlerFunc {
	return h.Handle(next).ServeHTTP
}

// Handle wraps next.
func (h *Handler) Handle(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := faultline.NewContext(r.Context())
		scope := faultline.ScopeFromContext(ctx)
		scope.SetHTTPRequest(r)
		if !h.opts.DisableBreadcrumb {
			path := ""
			if r.URL != nil {
				path = r.URL.Path
			}
			scope.AddBreadcrumb(faultline.Breadcrumb{
				Type:     faultline.BreadcrumbHTTP,
				Category: "request",
				Message:  r.Method + " " + path,
				Data:     map[string]any{"method": r.Method, "url": path},
			})
		}
		r = r.WithContext(ctx)
		sw := &statusWriter{ResponseWriter: w}

		defer func() {
			v := recover()
			if v == nil {
				return
			}
			if v == http.ErrAbortHandler { //nolint:errorlint // sentinel compared by identity, as net/http does
				panic(v)
			}
			faultline.CapturePanic(ctx, v)
			if h.opts.Repanic {
				panic(v)
			}
			if !sw.wroteHeader {
				http.Error(sw, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
			}
		}()

		next.ServeHTTP(sw, r)

		status := sw.status
		if !sw.wroteHeader {
			status = http.StatusOK
		}
		if h.shouldCapture(status) {
			path := ""
			if r.URL != nil {
				path = r.URL.Path
			}
			scope.SetTag("http.status_code", fmt.Sprint(status))
			faultline.CaptureMessageContext(ctx, fmt.Sprintf("HTTP %d %s: %s %s", status, http.StatusText(status), r.Method, path), faultline.LevelError)
		}
	})
}

func (h *Handler) shouldCapture(status int) (capture bool) {
	if h.opts.CaptureStatus == nil {
		return status >= 500
	}
	defer func() {
		if recover() != nil {
			capture = status >= 500
		}
	}()
	return h.opts.CaptureStatus(status)
}

// statusWriter records the status code. It implements Flusher and Hijacker when the wrapped
// writer does, and Unwrap for http.ResponseController.
type statusWriter struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (w *statusWriter) WriteHeader(code int) {
	if !w.wroteHeader {
		w.status = code
		w.wroteHeader = code >= 200 || code == http.StatusSwitchingProtocols
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if !w.wroteHeader {
		w.status = http.StatusOK
		w.wroteHeader = true
	}
	return w.ResponseWriter.Write(b)
}

func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		if !w.wroteHeader {
			w.status = http.StatusOK
			w.wroteHeader = true
		}
		f.Flush()
	}
}

func (w *statusWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if hj, ok := w.ResponseWriter.(http.Hijacker); ok {
		return hj.Hijack()
	}
	return nil, nil, fmt.Errorf("faultlinehttp: %T does not implement http.Hijacker", w.ResponseWriter)
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
