package domain

import (
	"reflect"
	"testing"
	"time"
)

func TestRegexesMatchContract(t *testing.T) {
	if SensitiveKeyPattern != `(authorization|cookie|set-cookie|password|passwd|secret|token|api[_-]?key|x-faultline-key)` {
		t.Fatalf("sensitive key regex drifted: %s", SensitiveKeyPattern)
	}
	if CreditCardPattern != `\b(?:\d[ -]*?){13,16}\b` {
		t.Fatalf("credit card regex drifted: %s", CreditCardPattern)
	}
}

func TestSensitiveKeys(t *testing.T) {
	for _, key := range []string{
		"Authorization", "COOKIE", "set-cookie", "password", "user_passwd", "clientSecret",
		"accessToken", "api_key", "apiKey", "API-KEY", "X-Faultline-Key",
	} {
		got := ScrubValue(map[string]any{key: "s3cr3t", "safe": "ok"}, ScrubOptions{CreditCards: true})
		want := map[string]any{key: Filtered, "safe": "ok"}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s: got %v", key, got)
		}
	}
}

func TestScrubNestedCardsAndURLs(t *testing.T) {
	in := map[string]any{
		"a":   map[string]any{"b": []any{map[string]any{"password": "x"}, "4111 1111 1111 1111"}},
		"url": "https://x.io/cb?token=abc&page=2",
		"n":   5,
	}
	got := ScrubValue(in, ScrubOptions{CreditCards: true})
	want := map[string]any{
		"a":   map[string]any{"b": []any{map[string]any{"password": Filtered}, Filtered}},
		"url": "https://x.io/cb?token=" + Filtered + "&page=2",
		"n":   5,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v", got)
	}
	if in["url"] != "https://x.io/cb?token=abc&page=2" {
		t.Fatal("input mutated")
	}
	if s := ScrubURL("/x?api_key=1&q=hello"); s != "/x?api_key="+Filtered+"&q=hello" {
		t.Fatalf("ScrubURL: %s", s)
	}
	if s := ScrubURL("/x?q=1#frag"); s != "/x?q=1#frag" {
		t.Fatalf("ScrubURL changed safe URL: %s", s)
	}
}

func baseEvent() *Event {
	return &Event{
		EventID:   "6f1c1d1e-2b3a-4c5d-8e9f-0a1b2c3d4e5f",
		Timestamp: time.Now(),
		Level:     LevelError,
		Platform:  Platform,
		SDK:       SDKInfo{Name: SDKName, Version: "1"},
	}
}

func TestScrubEventWithoutPII(t *testing.T) {
	e := baseEvent()
	e.Request = &Request{
		URL: "http://api/x?secret=1",
		Headers: map[string]string{
			"Authorization": "Bearer t", "Cookie": "sid=1", "X-Forwarded-For": "1.2.3.4",
			"x-custom-token": "t", "accept": "json",
		},
		Query: map[string]string{"card": "5500-0000-0000-0004", "q": "ok"},
	}
	e.User = &User{ID: "u1", IPAddress: "1.2.3.4"}
	e.Tags = map[string]string{"api_key": "k", "region": "eu"}
	e.Extra = map[string]any{"password": "p", "cc": "4111111111111111", "fine": "yes"}
	e.Contexts = map[string]any{"db": map[string]any{"token": "t", "card": "4111111111111111"}}
	e.Breadcrumbs = []Breadcrumb{{Type: BreadcrumbHTTP, Data: map[string]any{"cookie": "c", "card": "4111 1111 1111 1111"}}}

	out := ScrubEvent(e, false)
	wantHeaders := map[string]string{"x-custom-token": Filtered, "accept": "json"}
	if !reflect.DeepEqual(out.Request.Headers, wantHeaders) {
		t.Errorf("headers: %v", out.Request.Headers)
	}
	if out.Request.URL != "http://api/x?secret="+Filtered {
		t.Errorf("url: %s", out.Request.URL)
	}
	if !reflect.DeepEqual(out.Request.Query, map[string]string{"card": Filtered, "q": "ok"}) {
		t.Errorf("query: %v", out.Request.Query)
	}
	if out.User.IPAddress != "" || out.User.ID != "u1" {
		t.Errorf("user: %+v", out.User)
	}
	if out.Tags["api_key"] != Filtered || out.Tags["region"] != "eu" {
		t.Errorf("tags: %v", out.Tags)
	}
	if out.Extra["password"] != Filtered || out.Extra["cc"] != Filtered || out.Extra["fine"] != "yes" {
		t.Errorf("extra: %v", out.Extra)
	}
	db := out.Contexts["db"].(map[string]any)
	if db["token"] != Filtered || db["card"] != "4111111111111111" {
		t.Errorf("contexts (keys only, no card scrubbing): %v", db)
	}
	data := out.Breadcrumbs[0].Data
	if data["cookie"] != Filtered || data["card"] != Filtered {
		t.Errorf("breadcrumb data: %v", data)
	}
	if e.Request.Headers["Cookie"] != "sid=1" || e.User.IPAddress == "" {
		t.Error("input event mutated")
	}
}

func TestScrubEventWithPII(t *testing.T) {
	e := baseEvent()
	e.Request = &Request{Headers: map[string]string{"Cookie": "sid=1", "X-Forwarded-For": "1.2.3.4"}}
	e.User = &User{IPAddress: "1.2.3.4"}
	out := ScrubEvent(e, true)
	if out.Request.Headers["Cookie"] != Filtered {
		t.Errorf("cookie value must still be filtered: %v", out.Request.Headers)
	}
	if out.Request.Headers["X-Forwarded-For"] != "1.2.3.4" {
		t.Errorf("forwarding header kept with PII: %v", out.Request.Headers)
	}
	if out.User == nil || out.User.IPAddress != "1.2.3.4" {
		t.Errorf("user ip kept with PII: %+v", out.User)
	}
}

func TestScrubDropsEmptyUser(t *testing.T) {
	e := baseEvent()
	e.User = &User{IPAddress: "1.2.3.4"}
	if out := ScrubEvent(e, false); out.User != nil {
		t.Fatalf("user should be removed: %+v", out.User)
	}
}
