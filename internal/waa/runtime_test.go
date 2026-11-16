package waa

import (
	"context"
	"strings"
	"testing"

	"github.com/Mag1cFall/AIStudio2API/internal/waa/mozjs"
)

func TestCamouLocaleSanitizationAndContextInit(t *testing.T) {
	cases := []struct {
		input string
		want  [3]string
	}{
		{"zh-CN", [3]string{"zh", "CN", "zh-CN"}},
		{"zh_CN.UTF-8", [3]string{"zh", "CN", "zh-CN"}},
		{"zh-CN\u200b", [3]string{"zh", "CN", "zh-CN"}},
		{"\ufeffen-US\u200e", [3]string{"en", "US", "en-US"}},
		{"zh-Hans-CN", [3]string{"zh", "US", "zh-US"}},
		{"", [3]string{"en", "US", "en-US"}},
		{"C", [3]string{"en", "US", "en-US"}},
		{"invalid_locale_string", [3]string{"en", "US", "en-US"}},
	}
	for _, tc := range cases {
		got := camouLocale(tc.input)
		if got != tc.want {
			t.Fatalf("camouLocale(%q) = %v, want %v", tc.input, got, tc.want)
		}
		mozjs.SetCamouLocale(got[2])
		cx, err := mozjs.NewContext(mozjs.Options{HeapBytes: 32 << 20, Locale: got[2]})
		if err != nil {
			t.Fatalf("NewContext(%v) error: %v", got, err)
		}
		cx.Interrupt()
		cx.Close()
		cx.Interrupt()
	}
}

func TestFirefox158ShapeAndRealmInit(t *testing.T) {
	shape, err := firefoxShapeJSON()
	if err != nil {
		t.Fatalf("firefoxShapeJSON() error: %v", err)
	}
	if !strings.Contains(shape, "Firefox/158.0") {
		t.Fatalf("expected Firefox/158.0 in firefoxShapeJSON")
	}
	rt, err := NewRuntime(context.Background(), Options{
		Challenge: Challenge{
			Program:         "prog",
			GlobalName:      "WAA_TEST",
			InterpreterURL:  "https://www.google.com/js/bg/test.js",
			InterpreterHash: "test",
		},
		Interpreter: `window.WAA_TEST = { a: function(prog, cb) { cb(function(done, opts) { done("ok:" + navigator.userAgent); }); } };`,
		Profile: Profile{
			PageURL:  "https://aistudio.google.com/prompts/new_chat",
			Locale:   "en-US",
			TimeZone: "America/Los_Angeles",
		},
	})
	if err != nil {
		t.Fatalf("NewRuntime() error: %v", err)
	}
	defer rt.Close()
	token, err := rt.Proof(context.Background(), "digest", "hello")
	if err != nil {
		t.Fatalf("Proof() error: %v", err)
	}
	if !strings.Contains(token, "Firefox/158.0") {
		t.Fatalf("unexpected token: %q", token)
	}
}
