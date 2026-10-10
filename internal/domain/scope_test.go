package domain

import (
	"fmt"
	"sync"
	"testing"
)

func TestScopeCloneIsIndependent(t *testing.T) {
	s := NewScope()
	s.SetTag("a", "1")
	s.SetUser(User{ID: "u"})
	s.SetContext("ctx", map[string]any{"k": "v"})
	s.SetRequest(&Request{Headers: map[string]string{"h": "1"}})
	s.AddBreadcrumb(Breadcrumb{Message: "one"}, 10)

	c := s.Clone()
	c.SetTag("a", "2")
	c.SetTag("b", "3")
	c.AddBreadcrumb(Breadcrumb{Message: "two"}, 10)
	c.SetUser(User{})
	c.SetContext("ctx", nil)

	d := s.Data()
	if d.Tags["a"] != "1" || len(d.Tags) != 1 || d.User.ID != "u" || len(d.Breadcrumbs) != 1 || d.Contexts["ctx"] == nil {
		t.Fatalf("parent changed: %+v", d)
	}
	cd := c.Data()
	if cd.Tags["a"] != "2" || !cd.User.IsZero() || len(cd.Breadcrumbs) != 2 || cd.Contexts["ctx"] != nil {
		t.Fatalf("clone: %+v", cd)
	}
	cd.Request.Headers["h"] = "mutated"
	if s.Data().Request.Headers["h"] != "1" {
		t.Fatal("snapshot shares request headers")
	}
}

func TestScopeBreadcrumbCap(t *testing.T) {
	s := NewScope()
	for i := 0; i < 10; i++ {
		s.AddBreadcrumb(Breadcrumb{Message: fmt.Sprint(i)}, 3)
	}
	d := s.Data()
	if len(d.Breadcrumbs) != 3 || d.Breadcrumbs[0].Message != "7" {
		t.Fatalf("%v", d.Breadcrumbs)
	}
	s.AddBreadcrumb(Breadcrumb{}, 0)
	if len(s.Data().Breadcrumbs) != 3 {
		t.Fatal("max 0 must not add")
	}
	for i := 0; i < 300; i++ {
		s.AddBreadcrumb(Breadcrumb{}, 1000)
	}
	if len(s.Data().Breadcrumbs) != MaxBreadcrumbs {
		t.Fatal("hard cap is 100")
	}
	s.Clear()
	if len(s.Data().Breadcrumbs) != 0 {
		t.Fatal("clear")
	}
}

func TestScopeConcurrentUse(t *testing.T) {
	s := NewScope()
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				s.SetTag(fmt.Sprint(i), fmt.Sprint(j))
				s.AddBreadcrumb(Breadcrumb{}, 50)
				_ = s.Clone().Data()
			}
		}(i)
	}
	wg.Wait()
	if len(s.Data().Tags) != 20 {
		t.Fatal("lost writes")
	}
}
