package domain

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
)

// Normalization caps: a single extra/context blob can never blow up serialization.
const (
	maxNormalizeDepth   = 8
	maxNormalizeBreadth = 200
)

// NormalizeValue converts an arbitrary Go value into JSON-safe data built only from
// map[string]any, []any, string, json.Number, bool and nil. Errors become their message;
// values that cannot be marshaled (channels, functions, NaN, cycles, panicking MarshalJSON)
// become "[unserializable T]". Depth and breadth are capped and long strings truncated.
func NormalizeValue(v any) any {
	if v == nil {
		return nil
	}
	if err, ok := v.(error); ok {
		return Truncate(SafeErrorString(err), MaxMessageLength)
	}
	raw, err := safeMarshal(v)
	if err != nil {
		return fmt.Sprintf("[unserializable %T]", v)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var out any
	if err := dec.Decode(&out); err != nil {
		return fmt.Sprintf("[unserializable %T]", v)
	}
	return capTree(out, 0)
}

// NormalizeMap normalizes each value of m independently, so one bad value does not
// discard the others. It returns nil for an empty map.
func NormalizeMap(m map[string]any) map[string]any {
	if len(m) == 0 {
		return nil
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make(map[string]any, len(m))
	for i, k := range keys {
		if i >= maxNormalizeBreadth {
			break
		}
		out[k] = NormalizeValue(m[k])
	}
	return out
}

func safeMarshal(v any) (raw []byte, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("marshal panicked: %v", r)
		}
	}()
	return json.Marshal(v)
}

// SafeErrorString calls err.Error(), recovering from panics (for example a nil pointer receiver).
func SafeErrorString(err error) (s string) {
	if err == nil {
		return ""
	}
	defer func() {
		if r := recover(); r != nil {
			s = fmt.Sprintf("[%T: Error() panicked]", err)
		}
	}()
	return err.Error()
}

func capTree(v any, depth int) any {
	switch t := v.(type) {
	case string:
		return Truncate(t, MaxMessageLength)
	case []any:
		if depth >= maxNormalizeDepth {
			return "[Array]"
		}
		if len(t) > maxNormalizeBreadth {
			t = t[:maxNormalizeBreadth]
		}
		out := make([]any, len(t))
		for i, item := range t {
			out[i] = capTree(item, depth+1)
		}
		return out
	case map[string]any:
		if depth >= maxNormalizeDepth {
			return "[Object]"
		}
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		out := make(map[string]any, len(t))
		for i, k := range keys {
			if i >= maxNormalizeBreadth {
				break
			}
			out[k] = capTree(t[k], depth+1)
		}
		return out
	}
	return v
}
