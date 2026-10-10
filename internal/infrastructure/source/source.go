// Package source reads source-code context around in-app stack frames from disk.
package source

import (
	"bytes"
	"container/list"
	"os"
	"strings"
	"sync"

	"github.com/getfaultline/faultline-go/internal/domain"
)

// Options configures a Reader. Zero values use the defaults.
type Options struct {
	ContextLines    int // default and max domain.ContextLines (5)
	MaxFileBytes    int64
	MaxCacheBytes   int
	MaxCacheEntries int
}

type entry struct {
	file  string
	lines []string // nil: unreadable or too large (negative cache entry)
	size  int
}

// Reader implements application.SourceContextReader with a size-capped LRU cache. Unreadable
// or oversized files are cached as negative entries. Safe for concurrent use.
type Reader struct {
	contextLines    int
	maxFileBytes    int64
	maxCacheBytes   int
	maxCacheEntries int

	mu         sync.Mutex
	order      *list.List // front = most recent
	items      map[string]*list.Element
	cacheBytes int
	readFile   func(string) ([]byte, error)
	stat       func(string) (os.FileInfo, error)
}

// New returns a Reader.
func New(o Options) *Reader {
	r := &Reader{
		contextLines:    o.ContextLines,
		maxFileBytes:    o.MaxFileBytes,
		maxCacheBytes:   o.MaxCacheBytes,
		maxCacheEntries: o.MaxCacheEntries,
		order:           list.New(),
		items:           map[string]*list.Element{},
		readFile:        os.ReadFile,
		stat:            os.Stat,
	}
	if r.contextLines <= 0 || r.contextLines > domain.ContextLines {
		r.contextLines = domain.ContextLines
	}
	if r.maxFileBytes <= 0 {
		r.maxFileBytes = 1 << 20
	}
	if r.maxCacheBytes <= 0 {
		r.maxCacheBytes = 8 << 20
	}
	if r.maxCacheEntries <= 0 {
		r.maxCacheEntries = 100
	}
	return r
}

// AddSourceContext returns a copy of frames with context added to in-app frames.
func (r *Reader) AddSourceContext(frames []domain.StackFrame) []domain.StackFrame {
	out := make([]domain.StackFrame, len(frames))
	for i, f := range frames {
		out[i] = r.enrich(f)
	}
	return out
}

// CachedFiles lists cached file names, most recent first (for tests).
func (r *Reader) CachedFiles() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var names []string
	for e := r.order.Front(); e != nil; e = e.Next() {
		names = append(names, e.Value.(*entry).file)
	}
	return names
}

func (r *Reader) enrich(f domain.StackFrame) domain.StackFrame {
	if !f.InApp || f.Lineno < 1 || f.Filename == "" {
		return f
	}
	lines := r.lines(f.Filename)
	idx := f.Lineno - 1
	if lines == nil || idx >= len(lines) {
		return f
	}
	clip := func(ls []string) []string {
		out := make([]string, len(ls))
		for i, l := range ls {
			out[i] = domain.Truncate(l, domain.MaxContextLineLen)
		}
		return out
	}
	start := idx - r.contextLines
	if start < 0 {
		start = 0
	}
	end := idx + 1 + r.contextLines
	if end > len(lines) {
		end = len(lines)
	}
	f.PreContext = clip(lines[start:idx])
	f.ContextLine = domain.Truncate(lines[idx], domain.MaxContextLineLen)
	f.PostContext = clip(lines[idx+1 : end])
	return f
}

func (r *Reader) lines(file string) []string {
	r.mu.Lock()
	if el, ok := r.items[file]; ok {
		r.order.MoveToFront(el)
		lines := el.Value.(*entry).lines
		r.mu.Unlock()
		return lines
	}
	r.mu.Unlock()

	var lines []string
	if info, err := r.stat(file); err == nil && info.Mode().IsRegular() && info.Size() <= r.maxFileBytes {
		if data, err := r.readFile(file); err == nil {
			data = bytes.ReplaceAll(data, []byte("\r\n"), []byte("\n"))
			lines = strings.Split(string(data), "\n")
		}
	}
	size := 0
	for _, l := range lines {
		size += len(l)
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if el, ok := r.items[file]; ok { // another goroutine won the race
		r.order.MoveToFront(el)
		return el.Value.(*entry).lines
	}
	r.items[file] = r.order.PushFront(&entry{file: file, lines: lines, size: size})
	r.cacheBytes += size
	for r.order.Len() > r.maxCacheEntries || r.cacheBytes > r.maxCacheBytes {
		last := r.order.Back()
		if last == nil {
			break
		}
		e := last.Value.(*entry)
		r.order.Remove(last)
		delete(r.items, e.file)
		r.cacheBytes -= e.size
	}
	return lines
}
