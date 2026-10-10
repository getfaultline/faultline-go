package faultline_test

import (
	"go/build"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const modulePath = "github.com/getfaultline/faultline-go"

// layerRule describes what a package may import (non-test files only).
type layerRule struct {
	name string
	// match reports whether the rule applies to a package path relative to the module root.
	match func(rel string) bool
	// allowedInternal lists module-relative prefixes the package may import from this module.
	allowedInternal []string
	// forbiddenStd lists standard-library packages (or prefixes ending in "/") that are banned.
	forbiddenStd []string
}

// ioStd are standard packages that perform or enable I/O; the inner layers stay pure.
var ioStd = []string{"os", "os/", "net", "net/http", "net/http/", "io/fs", "syscall", "runtime", "log", "log/", "path/filepath", "crypto/rand"}

var rules = []layerRule{
	{
		name:  "domain",
		match: func(rel string) bool { return rel == "internal/domain" },
		// domain imports nothing from the module and no I/O packages
		forbiddenStd: ioStd,
	},
	{
		name:            "application",
		match:           func(rel string) bool { return rel == "internal/application" },
		allowedInternal: []string{"internal/domain"},
		forbiddenStd:    ioStd,
	},
	{
		name:            "infrastructure",
		match:           func(rel string) bool { return strings.HasPrefix(rel, "internal/infrastructure/") },
		allowedInternal: []string{"internal/domain", "internal/application"},
	},
	{
		name:            "contracttest (test support)",
		match:           func(rel string) bool { return rel == "internal/contracttest" },
		allowedInternal: []string{},
	},
	{
		name:  "public API (composition root)",
		match: func(rel string) bool { return rel == "" },
		allowedInternal: []string{
			"internal/domain", "internal/application", "internal/infrastructure/",
		},
	},
	{
		name: "integrations",
		match: func(rel string) bool {
			return strings.HasPrefix(rel, "faultline") && !strings.Contains(rel, "/")
		},
		allowedInternal: []string{""}, // the public root package only
	},
	{
		name:            "examples",
		match:           func(rel string) bool { return strings.HasPrefix(rel, "examples/") },
		allowedInternal: []string{"", "faultlinehttp", "faultlineslog"},
	},
}

// checkImports returns violations for a package at module-relative path rel.
func checkImports(rel string, imports []string) []string {
	var rule *layerRule
	for i := range rules {
		if rules[i].match(rel) {
			rule = &rules[i]
			break
		}
	}
	if rule == nil {
		return []string{rel + ": no layer rule covers this package"}
	}
	var violations []string
	for _, imp := range imports {
		if imp == modulePath || strings.HasPrefix(imp, modulePath+"/") {
			target := strings.TrimPrefix(strings.TrimPrefix(imp, modulePath), "/")
			ok := false
			for _, allowed := range rule.allowedInternal {
				if target == allowed || (strings.HasSuffix(allowed, "/") && strings.HasPrefix(target, allowed)) {
					ok = true
				}
			}
			if !ok {
				violations = append(violations, rel+" ("+rule.name+") must not import "+imp)
			}
			continue
		}
		for _, banned := range rule.forbiddenStd {
			if imp == banned || (strings.HasSuffix(banned, "/") && strings.HasPrefix(imp, banned)) {
				violations = append(violations, rel+" ("+rule.name+") must not import "+imp)
			}
		}
	}
	return violations
}

func TestArchitectureDependencyDirection(t *testing.T) {
	root, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	err = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return err
		}
		name := d.Name()
		if path != root && (strings.HasPrefix(name, ".") || name == "testdata") {
			return filepath.SkipDir
		}
		if path != root {
			if _, err := os.Stat(filepath.Join(path, "go.mod")); err == nil {
				return filepath.SkipDir // nested module (faultlinegin) has its own test
			}
		}
		pkg, err := build.Default.ImportDir(path, 0)
		if err != nil {
			if _, ok := err.(*build.NoGoError); ok {
				return nil
			}
			return err
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		if rel == "." {
			rel = ""
		}
		for _, v := range checkImports(rel, pkg.Imports) {
			t.Error(v)
		}
		checked++
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if checked < 10 {
		t.Fatalf("only %d packages checked; walker is broken", checked)
	}
}

// The rules must actually catch violations (guards against a vacuous test).
func TestArchitectureRulesCatchViolations(t *testing.T) {
	bad := map[string][]string{
		"internal/domain":                   {modulePath + "/internal/application", "os"},
		"internal/application":              {modulePath + "/internal/infrastructure/transport", "net/http"},
		"internal/infrastructure/transport": {modulePath},
		"faultlinehttp":                     {modulePath + "/internal/domain"},
	}
	for rel, imports := range bad {
		if got := checkImports(rel, imports); len(got) != len(imports) {
			t.Errorf("%s: expected %d violations, got %v", rel, len(imports), got)
		}
	}
	if got := checkImports("internal/application", []string{modulePath + "/internal/domain", "context", "sync"}); len(got) != 0 {
		t.Errorf("allowed imports flagged: %v", got)
	}
}
