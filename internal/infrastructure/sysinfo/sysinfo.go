// Package sysinfo provides the host adapters: runtime info, clock, ids, randomness, logger.
package sysinfo

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	mrand "math/rand"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"
)

// Runtime implements application.RuntimeInfo for the Go runtime.
type Runtime struct {
	once     sync.Once
	hostname string
}

// Contexts returns contexts.runtime ({name: "go", version}) and contexts.os ({name}).
func (r *Runtime) Contexts() map[string]any {
	return map[string]any{
		"runtime": map[string]any{"name": "go", "version": strings.TrimPrefix(runtime.Version(), "go")},
		"os":      map[string]any{"name": runtime.GOOS},
	}
}

// ServerName returns the hostname (cached).
func (r *Runtime) ServerName() string {
	r.once.Do(func() {
		if h, err := os.Hostname(); err == nil {
			r.hostname = h
		}
	})
	return r.hostname
}

// Clock is the system clock.
type Clock struct{}

// Now returns time.Now().
func (Clock) Now() time.Time { return time.Now() }

// IDs generates uuid v4 strings from crypto/rand.
type IDs struct{}

// NewID returns a lowercase uuid v4 with dashes.
func (IDs) NewID() string { return NewUUID() }

// NewUUID returns a lowercase uuid v4 with dashes.
func NewUUID() string {
	var b [16]byte
	if _, err := io.ReadFull(rand.Reader, b[:]); err != nil {
		// crypto/rand failing is practically impossible; fall back to math/rand.
		for i := range b {
			b[i] = byte(mrand.Intn(256)) //nolint:gosec // fallback only
		}
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

// Random is a math/rand source (safe for concurrent use).
type Random struct{}

// Float64 returns a uniform float in [0, 1).
func (Random) Float64() float64 { return mrand.Float64() } //nolint:gosec // sampling only

// Logger writes to stderr with a "[faultline] " prefix, only when enabled. It deliberately
// does not use the log or slog defaults, so the slog integration can never loop on SDK logs.
type Logger struct {
	enabled bool
	l       *log.Logger
}

// NewLogger returns a logger that writes to w (stderr when nil) only when enabled.
func NewLogger(enabled bool, w io.Writer) *Logger {
	if w == nil {
		w = os.Stderr
	}
	return &Logger{enabled: enabled, l: log.New(w, "[faultline] ", log.LstdFlags)}
}

// Debugf implements application.Logger.
func (l *Logger) Debugf(format string, args ...any) { l.out("debug", format, args) }

// Warnf implements application.Logger.
func (l *Logger) Warnf(format string, args ...any) { l.out("warn", format, args) }

// Errorf implements application.Logger.
func (l *Logger) Errorf(format string, args ...any) { l.out("error", format, args) }

func (l *Logger) out(level, format string, args []any) {
	if l == nil || !l.enabled {
		return
	}
	defer func() { _ = recover() }()
	l.l.Printf("%s: %s", level, fmt.Sprintf(format, args...))
}
