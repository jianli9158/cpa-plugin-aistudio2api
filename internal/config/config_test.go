package config

import (
	"testing"
	"time"
)

// TestValidateProxyCredentials 验证代理 URL 接受账号密码并拒绝空用户名
func TestValidateProxyCredentials(t *testing.T) {
	for _, value := range []string{"http://user:secret@127.0.0.1:8080", "socks5://user:secret@127.0.0.1:1080"} {
		if err := ValidateProxy(value); err != nil {
			t.Errorf("ValidateProxy(%q) = %v", value, err)
		}
	}
	if err := ValidateProxy("http://:secret@127.0.0.1:8080"); err == nil {
		t.Error("空用户名被接受")
	}
}

func TestParseFirstEventHedgeSupportsDurationStrings(t *testing.T) {
	for _, tc := range []struct {
		input string
		auto  bool
		after time.Duration
	}{
		{"auto", true, 0},
		{"off", false, 0},
		{"60", false, 60 * time.Second},
		{"60s", false, 60 * time.Second},
		{"6s", false, 6 * time.Second},
	} {
		auto, after, err := ParseFirstEventHedge(tc.input)
		if err != nil || auto != tc.auto || after != tc.after {
			t.Fatalf("ParseFirstEventHedge(%q) = (%v, %v, %v), want (%v, %v, nil)", tc.input, auto, after, err, tc.auto, tc.after)
		}
	}
}

func TestMaxRegenerationsConfig(t *testing.T) {
	cfg := Default()
	if cfg.MaxRegenerations != DefaultMaxRegenerations {
		t.Fatalf("Default().MaxRegenerations = %d, want %d", cfg.MaxRegenerations, DefaultMaxRegenerations)
	}
	for _, tc := range []struct {
		raw  string
		want int
	}{
		{"0", 0},
		{"5", 5},
	} {
		c := Default()
		if err := configSetters["MAX_REGENERATIONS"](&c, tc.raw); err != nil {
			t.Fatalf("set MAX_REGENERATIONS=%q failed: %v", tc.raw, err)
		}
		if c.MaxRegenerations != tc.want {
			t.Fatalf("MAX_REGENERATIONS=%q got %d, want %d", tc.raw, c.MaxRegenerations, tc.want)
		}
	}
	cBad := Default()
	if err := configSetters["MAX_REGENERATIONS"](&cBad, "abc"); err == nil {
		t.Fatalf("expected error for non-integer MAX_REGENERATIONS, got nil")
	}
	cNeg := Default()
	if err := configSetters["MAX_REGENERATIONS"](&cNeg, "-1"); err != nil {
		t.Fatalf("unexpected setter error for -1: %v", err)
	}
	if err := cNeg.Validate(); err == nil {
		t.Fatalf("expected Validate() error for MAX_REGENERATIONS=-1, got nil")
	}
}
