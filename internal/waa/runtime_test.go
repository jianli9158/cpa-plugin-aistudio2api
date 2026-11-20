package waa

import (
	"context"
	"runtime"
	"strings"
	"testing"

	"github.com/Mag1cFall/AIStudio2API/internal/waa/mozjs"
)

func TestCamouLocaleSanitizationAndContextInit(t *testing.T) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
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

func TestStencilCacheBoundedLRUAndClear(t *testing.T) {
	runtimeLock := make(chan struct{})
	go func() {
		defer close(runtimeLock)
		// mozjs.Context is bound to its creating OS thread via NewRuntime/eventLoop or explicit LockOSThread
		mozjs.ClearStencilCache()
		entries0, bytes0 := mozjs.StencilCacheStats()
		if entries0 != 0 || bytes0 != 0 {
			t.Errorf("expected empty stencil cache after ClearStencilCache, got entries=%d bytes=%d", entries0, bytes0)
			return
		}
	}()
	<-runtimeLock

	rt, err := NewRuntime(context.Background(), Options{
		Challenge: Challenge{
			Program:         "prog",
			GlobalName:      "WAA_LRU",
			InterpreterURL:  "https://www.google.com/js/bg/lru.js",
			InterpreterHash: "lru",
		},
		Interpreter: `window.WAA_LRU = { a: function(prog, cb) { cb(function(done) { done("lru_ok"); }); } };`,
	})
	if err != nil {
		t.Fatalf("NewRuntime() error: %v", err)
	}
	entriesAfterInit, bytesAfterInit := mozjs.StencilCacheStats()
	if entriesAfterInit == 0 || bytesAfterInit == 0 {
		rt.Close()
		t.Fatalf("expected non-empty stencil cache after runtime init, got entries=%d bytes=%d", entriesAfterInit, bytesAfterInit)
	}
	rt.Close()

	// Evaluate >48 distinct scripts via isolated runtimes to verify hard LRU cap (48 entries)
	for i := range 55 {
		r, rErr := NewRuntime(context.Background(), Options{
			Challenge: Challenge{
				Program:         "p",
				GlobalName:      "WAA_CAP",
				InterpreterURL:  "https://www.google.com/js/bg/cap.js",
				InterpreterHash: "cap",
			},
			Interpreter: `window.WAA_CAP = { idx: ` + string(rune('0'+i%10)) + `, tag: "` + strings.Repeat("x", i+1) + `", a: function(prog, cb) { cb(function(done) { done("ok"); }); } };`,
		})
		if rErr != nil {
			t.Fatalf("NewRuntime(%d) error: %v", i, rErr)
		}
		r.Close()
	}
	cappedEntries, cappedBytes := mozjs.StencilCacheStats()
	if cappedEntries > 48 {
		t.Fatalf("expected stencil cache entries <= 48, got %d (bytes=%d)", cappedEntries, cappedBytes)
	}
	mozjs.ClearStencilCache()
	clearedEntries, clearedBytes := mozjs.StencilCacheStats()
	if clearedEntries != 0 || clearedBytes != 0 {
		t.Fatalf("expected 0 entries/bytes after ClearStencilCache, got entries=%d bytes=%d", clearedEntries, clearedBytes)
	}
}
