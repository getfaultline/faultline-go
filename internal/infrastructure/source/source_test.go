package source

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/getfaultline/faultline-go/internal/domain"
)

func writeFile(t *testing.T, dir, name string, lines int, lineLen int) string {
	t.Helper()
	var b strings.Builder
	for i := 1; i <= lines; i++ {
		fmt.Fprintf(&b, "line %d %s\n", i, strings.Repeat("x", lineLen))
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestAddsContextToInAppFrames(t *testing.T) {
	dir := t.TempDir()
	file := writeFile(t, dir, "a.go", 20, 0)
	r := New(Options{})
	out := r.AddSourceContext([]domain.StackFrame{
		{Filename: file, Lineno: 10, InApp: true},
		{Filename: file, Lineno: 2, InApp: true},
		{Filename: file, Lineno: 10, InApp: false},
		{Filename: file, Lineno: 99, InApp: true},
		{Filename: filepath.Join(dir, "missing.go"), Lineno: 1, InApp: true},
	})
	f := out[0]
	if f.ContextLine != "line 10 " || len(f.PreContext) != 5 || f.PreContext[0] != "line 5 " || len(f.PostContext) != 5 || f.PostContext[4] != "line 15 " {
		t.Fatalf("context: %+v", f)
	}
	if len(out[1].PreContext) != 1 || out[1].ContextLine != "line 2 " {
		t.Fatalf("near start: %+v", out[1])
	}
	if out[2].ContextLine != "" || out[3].ContextLine != "" || out[4].ContextLine != "" {
		t.Fatal("non in-app, out of range and missing files get no context")
	}
	if len(r.CachedFiles()) != 2 {
		t.Fatalf("files (including negative entry) are cached: %v", r.CachedFiles())
	}
}

func TestTruncatesLongLines(t *testing.T) {
	file := writeFile(t, t.TempDir(), "long.go", 3, 1000)
	out := New(Options{}).AddSourceContext([]domain.StackFrame{{Filename: file, Lineno: 2, InApp: true}})
	if n := len([]rune(out[0].ContextLine)); n != domain.MaxContextLineLen {
		t.Fatalf("len %d", n)
	}
}

func TestSkipsLargeFilesAndEvicts(t *testing.T) {
	dir := t.TempDir()
	big := writeFile(t, dir, "big.go", 100, 100)
	r := New(Options{MaxFileBytes: 1000, MaxCacheEntries: 2})
	out := r.AddSourceContext([]domain.StackFrame{{Filename: big, Lineno: 1, InApp: true}})
	if out[0].ContextLine != "" {
		t.Fatal("large file should be skipped")
	}
	for i := 0; i < 3; i++ {
		f := writeFile(t, dir, fmt.Sprintf("f%d.go", i), 3, 0)
		r.AddSourceContext([]domain.StackFrame{{Filename: f, Lineno: 1, InApp: true}})
	}
	files := r.CachedFiles()
	if len(files) != 2 || !strings.HasSuffix(files[0], "f2.go") {
		t.Fatalf("LRU eviction: %v", files)
	}
}
