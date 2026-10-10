// Package stack converts Go program counters into contract stack frames and extracts stacks
// carried by errors (pkg/errors-style StackTrace(), go-errors-style Callers()).
package stack

import (
	"path/filepath"
	"reflect"
	"runtime"
	"strings"

	"github.com/getfaultline/faultline-go/internal/domain"
)

// Parser implements application.StackParser.
type Parser struct {
	gorootSrc string
	// trimmed is true for binaries built with -trimpath: file names are then relative
	// ("net/http/server.go", "github.com/x/y@v1.2.3/z.go", "example.com/app/main.go").
	trimmed bool
}

// New returns a parser that marks frames in GOROOT or the module cache as not in-app.
func New() *Parser {
	src, trimmed := detectGorootSrc()
	return &Parser{gorootSrc: src, trimmed: trimmed}
}

// NewWithGoroot is New with an explicit GOROOT/src prefix (for tests).
func NewWithGoroot(gorootSrc string) *Parser { return &Parser{gorootSrc: gorootSrc} }

// detectGorootSrc derives "<GOROOT>/src/" from the file of a runtime function, which works even
// when the GOROOT used at build time is not the one on this machine's PATH. It falls back to
// GOROOT detected at startup.
func detectGorootSrc() (src string, trimmed bool) {
	pcs := make([]uintptr, 1)
	if runtime.Callers(0, pcs) > 0 {
		frame, _ := runtime.CallersFrames(pcs).Next()
		file := filepath.ToSlash(frame.File)
		if strings.HasPrefix(file, "runtime/") {
			return "", true
		}
		if i := strings.LastIndex(file, "/src/runtime/"); i >= 0 {
			return file[:i+len("/src/")], false
		}
	}
	if root := runtime.GOROOT(); root != "" {
		return strings.TrimSuffix(filepath.ToSlash(root), "/") + "/src/", false
	}
	return "", false
}

// InApp reports whether file belongs to the application: not in GOROOT and not under /pkg/mod/.
func (p *Parser) InApp(file string) bool {
	f := filepath.ToSlash(file)
	if f == "" || strings.HasPrefix(f, "<") {
		return false
	}
	if p.gorootSrc != "" && strings.HasPrefix(f, p.gorootSrc) {
		return false
	}
	if strings.Contains(f, "/pkg/mod/") {
		return false
	}
	if p.trimmed && !strings.HasPrefix(f, "/") && !isWindowsAbs(f) {
		// -trimpath: standard library paths have no dot in their first element and module
		// dependencies carry an "@version" element.
		first, _, _ := strings.Cut(f, "/")
		if !strings.Contains(first, ".") || strings.Contains(f, "@v") {
			return false
		}
	}
	return true
}

// Frames converts return PCs (newest first) into frames ordered oldest to newest, keeping the
// newest domain.MaxFrames. With panicking, everything newer than runtime.gopanic is dropped.
func (p *Parser) Frames(pcs []uintptr, panicking bool) []domain.StackFrame {
	if len(pcs) == 0 {
		return []domain.StackFrame{}
	}
	iter := runtime.CallersFrames(pcs)
	var raw []runtime.Frame // newest first
	for i := 0; i < 4096; i++ {
		f, more := iter.Next()
		if f.Function != "" || f.File != "" {
			raw = append(raw, f)
		}
		if !more {
			break
		}
	}
	if panicking {
		for i, f := range raw {
			if f.Function == "runtime.gopanic" {
				raw = raw[i+1:]
				// Runtime faults (nil dereference, bad map write, ...) panic from runtime
				// helpers such as runtime.panicmem and runtime.sigpanic; drop them so the
				// newest frame is the code that faulted.
				for len(raw) > 1 && strings.HasPrefix(raw[0].Function, "runtime.") {
					raw = raw[1:]
				}
				break
			}
		}
	}
	if len(raw) > domain.MaxFrames {
		raw = raw[:domain.MaxFrames]
	}
	out := make([]domain.StackFrame, len(raw))
	for i, f := range raw {
		module, function := SplitFunction(f.Function)
		out[len(raw)-1-i] = domain.StackFrame{
			Filename: f.File,
			Function: function,
			Module:   module,
			Lineno:   f.Line,
			InApp:    p.InApp(f.File),
		}
	}
	return out
}

// SplitFunction splits a runtime function name into its package import path and the
// package-qualified function: "github.com/a/b.(*T).m" becomes ("github.com/a/b", "b.(*T).m"),
// and "main.main.func1" becomes ("main", "main.main.func1").
func SplitFunction(name string) (module, function string) {
	if name == "" {
		return "", ""
	}
	slash := strings.LastIndex(name, "/")
	dot := strings.Index(name[slash+1:], ".")
	if dot < 0 {
		return "", name
	}
	dot += slash + 1
	return name[:dot], name[slash+1:]
}

// FromError returns the program counters err carries itself, or nil. Supported shapes:
// StackTrace() returning a slice of uintptr-kinded values (github.com/pkg/errors,
// faultline.WithStack) and Callers() []uintptr (github.com/go-errors/errors).
func (p *Parser) FromError(err error) (pcs []uintptr) {
	defer func() {
		if recover() != nil {
			pcs = nil
		}
	}()
	if err == nil {
		return nil
	}
	if c, ok := err.(interface{ Callers() []uintptr }); ok {
		return c.Callers()
	}
	v := reflect.ValueOf(err)
	if v.Kind() == reflect.Ptr && v.IsNil() {
		return nil
	}
	m := v.MethodByName("StackTrace")
	if !m.IsValid() {
		return nil
	}
	t := m.Type()
	if t.NumIn() != 0 || t.NumOut() != 1 {
		return nil
	}
	out := t.Out(0)
	if out.Kind() != reflect.Slice || out.Elem().Kind() != reflect.Uintptr {
		return nil
	}
	res := m.Call(nil)[0]
	n := res.Len()
	if n == 0 {
		return nil
	}
	pcs = make([]uintptr, n)
	for i := 0; i < n; i++ {
		pcs[i] = uintptr(res.Index(i).Uint())
	}
	return pcs
}

func isWindowsAbs(f string) bool {
	return len(f) >= 3 && f[1] == ':' && f[2] == '/'
}
