// Package faultlinegin is Gin middleware for Faultline. It lives in its own module so the core
// SDK stays dependency-free.
//
//	r := gin.New()
//	r.Use(faultlinegin.New(faultlinegin.Options{}))
//
// Every request gets its own scope (carried by c.Request.Context()), request data and an
// "http" breadcrumb. Panics are captured at level "fatal" and answered with a 500 (or
// re-panicked with Repanic). After the handler chain, errors added with c.Error are captured
// when the response status is >= 500, and a 5xx without errors is reported as a message.
package faultlinegin

import (
	"fmt"
	"net/http"

	faultline "github.com/getfaultline/faultline-go"
	"github.com/gin-gonic/gin"
)

// Options configures the middleware.
type Options struct {
	// Repanic re-panics after capturing so gin.Recovery (or another handler) can respond.
	// By default the middleware aborts with 500.
	Repanic bool
	// CaptureStatus decides which response statuses are reported when the chain returns
	// normally. Default: status >= 500.
	CaptureStatus func(status int) bool
	// DisableBreadcrumb skips the "http" breadcrumb for the incoming request.
	DisableBreadcrumb bool
}

// New returns the middleware.
func New(opts Options) gin.HandlerFunc {
	return func(c *gin.Context) {
		ctx := faultline.NewContext(c.Request.Context())
		scope := faultline.ScopeFromContext(ctx)
		scope.SetHTTPRequest(c.Request)
		if !opts.DisableBreadcrumb {
			scope.AddBreadcrumb(faultline.Breadcrumb{
				Type:     faultline.BreadcrumbHTTP,
				Category: "request",
				Message:  c.Request.Method + " " + c.Request.URL.Path,
				Data:     map[string]any{"method": c.Request.Method, "url": c.Request.URL.Path, "route": c.FullPath()},
			})
		}
		if route := c.FullPath(); route != "" {
			scope.SetTag("route", route)
		}
		c.Request = c.Request.WithContext(ctx)

		defer func() {
			v := recover()
			if v == nil {
				return
			}
			if v == http.ErrAbortHandler { //nolint:errorlint // sentinel compared by identity, as net/http does
				panic(v)
			}
			faultline.CapturePanic(ctx, v)
			if opts.Repanic {
				panic(v)
			}
			c.AbortWithStatus(http.StatusInternalServerError)
		}()

		c.Next()

		status := c.Writer.Status()
		if !shouldCapture(opts, status) {
			return
		}
		scope.SetTag("http.status_code", fmt.Sprint(status))
		if last := c.Errors.Last(); last != nil && last.Err != nil {
			faultline.CaptureExceptionContext(ctx, last.Err)
			return
		}
		faultline.CaptureMessageContext(ctx, fmt.Sprintf("HTTP %d %s: %s %s", status, http.StatusText(status), c.Request.Method, c.Request.URL.Path), faultline.LevelError)
	}
}

// GetScope returns the request scope created by the middleware (nil without it). All Scope
// methods are no-ops on nil.
func GetScope(c *gin.Context) *faultline.Scope {
	if c == nil || c.Request == nil {
		return nil
	}
	return faultline.ScopeFromContext(c.Request.Context())
}

func shouldCapture(opts Options, status int) (capture bool) {
	if opts.CaptureStatus == nil {
		return status >= 500
	}
	defer func() {
		if recover() != nil {
			capture = status >= 500
		}
	}()
	return opts.CaptureStatus(status)
}
