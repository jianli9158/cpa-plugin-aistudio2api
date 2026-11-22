package api

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return fn(req)
}

func TestCheckPublicIPBlocksAllNonPublicRanges(t *testing.T) {
	blocked := []string{
		"127.0.0.1",
		"127.255.255.254",
		"::1",
		"0.0.0.0",
		"0.0.0.1",
		"::",
		"10.0.0.1",
		"172.16.0.1",
		"192.168.1.1",
		"169.254.169.254",
		"100.64.0.1",
		"100.127.255.254",
		"192.0.0.1",
		"192.0.2.1",
		"198.18.0.1",
		"198.51.100.1",
		"203.0.113.1",
		"224.0.0.1",
		"240.0.0.1",
		"255.255.255.255",
		"fc00::1",
		"fd12:3456::1",
		"fe80::1",
		"fe80::1%eth0",
		"::ffff:127.0.0.1",
		"::ffff:169.254.169.254",
		"::ffff:10.0.0.1",
		"::ffff:100.64.0.1",
		"64:ff9b::127.0.0.1",
		"64:ff9b:1::10.0.0.1",
		"2002:7f00:0001::1",
		"2001:db8::1",
		"100::1",
		"ff02::1",
	}
	for _, raw := range blocked {
		addr, err := netip.ParseAddr(raw)
		if err != nil {
			t.Fatalf("ParseAddr(%q): %v", raw, err)
		}
		if err := CheckPublicIP(addr); !errors.Is(err, errOutboundBlocked) {
			t.Errorf("CheckPublicIP(%q) = %v, want errOutboundBlocked", raw, err)
		}
	}

	allowed := []string{
		"1.1.1.1",
		"8.8.8.8",
		"93.184.216.34",
		"2606:2800:220:1:248:1893:25c8:1946",
	}
	for _, raw := range allowed {
		addr := netip.MustParseAddr(raw)
		if err := CheckPublicIP(addr); err != nil {
			t.Errorf("CheckPublicIP(%q) unexpected error: %v", raw, err)
		}
	}
}

func TestOutboundAllowlistParsingAndMatching(t *testing.T) {
	if !ParseOutboundAllowlist("").IsEmpty() {
		t.Fatal("empty allowlist must report IsEmpty() == true")
	}
	// Dangerous/invalid rules must all be ignored.
	bad := ParseOutboundAllowlist("*, http://*, https://*, *.com, 127.0.0.1, http://127.0.0.1:8317, 169.254.169.254, https://user:pass@mcp.example.com, https://mcp.example.com/path, ftp://mcp.example.com")
	if !bad.IsEmpty() {
		t.Fatalf("expected all unsafe rules to be ignored, got %+v", bad.rules)
	}

	list := ParseOutboundAllowlist("mcp.example.com, mcp.example.com:8443, *.trusted.example.com, http://plain.example.org:8080")
	cases := []struct {
		raw  string
		want bool
	}{
		{"https://mcp.example.com/mcp", true},
		{"https://MCP.EXAMPLE.COM./mcp", true},
		{"https://mcp.example.com:443/mcp", true},
		{"http://mcp.example.com/mcp", false},
		{"https://mcp.example.com:8443/mcp", true},
		{"https://mcp.example.com:9443/mcp", false},
		{"https://sub.trusted.example.com/mcp", true},
		{"https://a.b.trusted.example.com/mcp", true},
		{"https://trusted.example.com/mcp", false},
		{"https://eviltrusted.example.com/mcp", false},
		{"http://sub.trusted.example.com/mcp", false},
		{"http://plain.example.org:8080/mcp", true},
		{"https://plain.example.org:8080/mcp", false},
		{"http://plain.example.org/mcp", false},
		{"https://other.example.com/mcp", false},
	}
	for _, tc := range cases {
		u, err := url.Parse(tc.raw)
		if err != nil {
			t.Fatalf("url.Parse(%q): %v", tc.raw, err)
		}
		if got := list.Allows(u); got != tc.want {
			t.Errorf("Allows(%q) = %v, want %v", tc.raw, got, tc.want)
		}
	}
}

func TestMCPSSRFBlockedAcrossResponsesAndAnthropic(t *testing.T) {
	var hits atomic.Int32
	victim := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"secret":"cpa-management-data"}`))
	}))
	defer victim.Close()

	victimURL, err := url.Parse(victim.URL)
	if err != nil {
		t.Fatalf("parse victim URL: %v", err)
	}

	for _, tc := range []struct {
		name      string
		allowlist string
		path      string
		body      string
	}{
		{
			name:      "responses_default_approval_empty_allowlist",
			allowlist: "",
			path:      "/v1/responses",
			body:      `{"model":"gemini-3.8-flash","input":"hi","tools":[{"type":"mcp","server_label":"local","server_url":"` + victim.URL + `","headers":{"X-Management-Key":"secret"}}]}`,
		},
		{
			name:      "responses_loopback_even_if_allowlisted",
			allowlist: "http://" + victimURL.Host + ", http://localhost:" + victimURL.Port(),
			path:      "/v1/responses",
			body:      `{"model":"gemini-3.8-flash","input":"hi","tools":[{"type":"mcp","server_label":"local","server_url":"` + victim.URL + `"}]}`,
		},
		{
			name:      "responses_read_only_resolve_mcp_tools",
			allowlist: "",
			path:      "/v1/responses",
			body:      `{"model":"gemini-3.8-flash","input":"hi","tools":[{"type":"mcp","server_label":"local","server_url":"` + victim.URL + `","require_approval":"never","allowed_tools":{"read_only":true}}]}`,
		},
		{
			name:      "responses_approval_response_call_mcp_tool",
			allowlist: "",
			path:      "/v1/responses",
			body:      `{"model":"gemini-3.8-flash","input":[{"type":"mcp_approval_request","id":"mcpr_1","server_label":"local","name":"read","arguments":"{}"},{"type":"mcp_approval_response","approval_request_id":"mcpr_1","approve":true}],"tools":[{"type":"mcp","server_label":"local","server_url":"` + victim.URL + `"}]}`,
		},
		{
			name:      "anthropic_disabled_tools_resolve_mcp_tools",
			allowlist: "",
			path:      "/v1/messages",
			body:      `{"model":"gemini-3.8-flash","max_tokens":16,"messages":[{"role":"user","content":"hi"}],"mcp_servers":[{"type":"url","name":"local","url":"` + victim.URL + `"}],"tools":[{"type":"mcp_toolset","mcp_server_name":"local","configs":{"drop":{"enabled":false}}}]}`,
		},
		{
			name:      "responses_unallowlisted_public_host",
			allowlist: "mcp.trusted.example",
			path:      "/v1/responses",
			body:      `{"model":"gemini-3.8-flash","input":"hi","tools":[{"type":"mcp","server_label":"ext","server_url":"https://untrusted.example/mcp"}]}`,
		},
		{
			name:      "responses_userinfo_rejected",
			allowlist: "mcp.trusted.example",
			path:      "/v1/responses",
			body:      `{"model":"gemini-3.8-flash","input":"hi","tools":[{"type":"mcp","server_label":"ext","server_url":"https://user:pass@mcp.trusted.example/mcp"}]}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hits.Store(0)
			h := NewHandler(&stubService{}, Config{
				AllowedMCPHosts: func() string { return tc.allowlist },
			})
			req := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
			}
			if hits.Load() != 0 {
				t.Fatalf("expected 0 outbound hits to local server, got %d", hits.Load())
			}
		})
	}
}

func TestOutboundValidatorBlocksDNSRebindingAndMixedPrivateIPs(t *testing.T) {
	// 1. Mixed DNS response containing one public IP and one loopback IP must be rejected.
	mixedValidator := outboundValidator{
		kind:             "mcp",
		configKey:        "allowed_mcp_hosts",
		allowlist:        ParseOutboundAllowlist("dual.trusted.example"),
		requireAllowlist: true,
		lookupIP: func(ctx context.Context, host string) ([]netip.Addr, error) {
			return []netip.Addr{
				netip.MustParseAddr("93.184.216.34"),
				netip.MustParseAddr("127.0.0.1"),
			}, nil
		},
	}
	if _, _, err := mixedValidator.validateURL(t.Context(), "https://dual.trusted.example/mcp"); !errors.Is(err, errOutboundBlocked) {
		t.Fatalf("expected errOutboundBlocked for mixed DNS response, got %v", err)
	}

	// 2. DNS rebinding: pre-flight lookup returns public IP, dial-time lookup flips to 127.0.0.1.
	var victimHits atomic.Int32
	victim := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		victimHits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer victim.Close()
	victimURL, _ := url.Parse(victim.URL)

	var lookupCalls atomic.Int32
	rebindingValidator := outboundValidator{
		kind:             "mcp",
		configKey:        "allowed_mcp_hosts",
		allowlist:        ParseOutboundAllowlist("http://rebind.trusted.example:" + victimURL.Port()),
		requireAllowlist: true,
		allowRedirects:   true,
		maxRedirects:     5,
		lookupIP: func(ctx context.Context, host string) ([]netip.Addr, error) {
			if lookupCalls.Add(1) == 1 {
				return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
			}
			return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
		},
	}
	client, err := rebindingValidator.newHTTPClient(0, nil)
	if err != nil {
		t.Fatalf("newHTTPClient: %v", err)
	}
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://rebind.trusted.example:"+victimURL.Port()+"/mcp", nil)
	resp, err := client.Do(req)
	if resp != nil {
		resp.Body.Close()
	}
	if !errors.Is(err, errOutboundBlocked) {
		t.Fatalf("expected DNS rebinding to be blocked with errOutboundBlocked, got err=%v", err)
	}
	if victimHits.Load() != 0 {
		t.Fatalf("expected 0 hits on victim server during DNS rebinding, got %d", victimHits.Load())
	}

	// 3. Direct Dialer.Control check: dialing literal loopback directly must fail before connect(2).
	directTransport, err := rebindingValidator.newTransport()
	if err != nil {
		t.Fatalf("newTransport: %v", err)
	}
	conn, err := directTransport.DialContext(t.Context(), "tcp", victimURL.Host)
	if conn != nil {
		conn.Close()
	}
	if !errors.Is(err, errOutboundBlocked) {
		t.Fatalf("expected DialContext to reject loopback target with errOutboundBlocked, got %v", err)
	}
}

func TestMCPRedirectValidationAndCrossOriginHeaderStripping(t *testing.T) {
	dns := func(ctx context.Context, host string) ([]netip.Addr, error) {
		switch host {
		case "hop1.trusted.example", "hop2.trusted.example", "evil.example.net":
			return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
		case "internal.trusted.example":
			return []netip.Addr{netip.MustParseAddr("169.254.169.254")}, nil
		default:
			return nil, errors.New("unknown host")
		}
	}

	validator := outboundValidator{
		kind:             "mcp",
		configKey:        "allowed_mcp_hosts",
		allowlist:        ParseOutboundAllowlist("*.trusted.example"),
		requireAllowlist: true,
		allowRedirects:   true,
		maxRedirects:     5,
		lookupIP:         dns,
	}
	origin, _ := url.Parse("https://hop1.trusted.example/mcp")

	for _, tc := range []struct {
		name     string
		location string
	}{
		{"redirect_to_loopback", "http://127.0.0.1:8317/v0/management/config"},
		{"redirect_to_metadata_ip", "http://169.254.169.254/latest/meta-data/"},
		{"redirect_to_allowlisted_host_resolving_to_metadata", "https://internal.trusted.example/meta"},
		{"redirect_to_unallowlisted_public_host", "https://evil.example.net/steal"},
		{"redirect_to_http_downgrade", "http://hop2.trusted.example/mcp"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			base := roundTripFunc(func(req *http.Request) (*http.Response, error) {
				if calls.Add(1) == 1 {
					return &http.Response{
						StatusCode: http.StatusFound,
						Header:     http.Header{"Location": []string{tc.location}},
						Body:       io.NopCloser(bytes.NewReader(nil)),
						Request:    req,
					}, nil
				}
				t.Errorf("blocked redirect destination %s should never reach base RoundTrip", req.URL.String())
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(nil)), Request: req}, nil
			})
			client := validator.newHTTPClientWithBase(0, base, func(inner http.RoundTripper) http.RoundTripper {
				return mcpHeaderTransport{
					base:    inner,
					origin:  origin,
					headers: map[string]string{"Authorization": "Bearer secret", "X-Custom-Secret": "top-secret"},
				}
			})
			req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, origin.String(), nil)
			resp, err := client.Do(req)
			if resp != nil {
				resp.Body.Close()
			}
			if !errors.Is(err, errOutboundBlocked) {
				t.Fatalf("expected errOutboundBlocked for %s, got %v", tc.location, err)
			}
			if calls.Load() != 1 {
				t.Fatalf("expected only 1 round-trip before blocked redirect, got %d", calls.Load())
			}
		})
	}

	// Redirect to another allowlisted public origin must succeed AND strip Authorization + custom MCP headers.
	var hop2Auth, hop2Custom string
	base := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Host == "hop1.trusted.example" {
			if req.Header.Get("Authorization") != "Bearer secret" || req.Header.Get("X-Custom-Secret") != "top-secret" {
				t.Errorf("expected headers on initial origin, got auth=%q custom=%q", req.Header.Get("Authorization"), req.Header.Get("X-Custom-Secret"))
			}
			return &http.Response{
				StatusCode: http.StatusFound,
				Header:     http.Header{"Location": []string{"https://hop2.trusted.example/mcp"}},
				Body:       io.NopCloser(bytes.NewReader(nil)),
				Request:    req,
			}, nil
		}
		hop2Auth = req.Header.Get("Authorization")
		hop2Custom = req.Header.Get("X-Custom-Secret")
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader("ok")),
			Request:    req,
		}, nil
	})
	client := validator.newHTTPClientWithBase(0, base, func(inner http.RoundTripper) http.RoundTripper {
		return mcpHeaderTransport{
			base:    inner,
			origin:  origin,
			headers: map[string]string{"Authorization": "Bearer secret", "X-Custom-Secret": "top-secret"},
		}
	})
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, origin.String(), nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("allowlisted cross-origin redirect failed: %v", err)
	}
	resp.Body.Close()
	if hop2Auth != "" || hop2Custom != "" {
		t.Fatalf("expected credentials stripped on cross-origin redirect, got auth=%q custom=%q", hop2Auth, hop2Custom)
	}
}

func TestInteractionWebhookSSRFBlockedByDefaultAndRejectsRedirects(t *testing.T) {
	var hits atomic.Int32
	victim := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer victim.Close()

	// Default empty AllowedWebhookHosts must reject /v1/interactions webhook_config with 400.
	h := NewHandler(&stubService{}, Config{})
	body := `{"model":"gemini-3.8-flash","input":"hi","background":true,"webhook_config":{"uris":["` + victim.URL + `"]}}`
	req := httptest.NewRequest(http.MethodPost, "/v1/interactions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for blocked webhook_config, got %d body=%s", rec.Code, rec.Body.String())
	}
	if hits.Load() != 0 {
		t.Fatalf("expected 0 webhook hits, got %d", hits.Load())
	}

	// Webhook client must reject redirects even to allowlisted public destinations.
	validator := outboundValidator{
		kind:             "webhook",
		configKey:        "allowed_webhook_hosts",
		allowlist:        ParseOutboundAllowlist("hooks.trusted.example"),
		requireAllowlist: true,
		allowRedirects:   false,
		lookupIP: func(ctx context.Context, host string) ([]netip.Addr, error) {
			return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
		},
	}
	base := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusFound,
			Header:     http.Header{"Location": []string{"https://hooks.trusted.example/redirected"}},
			Body:       io.NopCloser(bytes.NewReader(nil)),
			Request:    req,
		}, nil
	})
	client := validator.newHTTPClientWithBase(0, base, nil)
	wreq, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, "https://hooks.trusted.example/events", strings.NewReader("{}"))
	resp, err := client.Do(wreq)
	if resp != nil {
		resp.Body.Close()
	}
	if !errors.Is(err, errOutboundBlocked) {
		t.Fatalf("expected webhook redirect to return errOutboundBlocked, got %v", err)
	}
}

func TestRemoteMediaBlocksInternalAndRedirectTargets(t *testing.T) {
	var hits atomic.Int32
	victim := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer victim.Close()

	for _, rawURL := range []string{
		victim.URL,
		"http://169.254.169.254/latest/meta-data/",
		"http://100.64.0.1/secret.png",
		"http://0.0.0.1/secret.png",
		"http://[::ffff:127.0.0.1]/secret.png",
		"http://user:pass@93.184.216.34/image.png",
		"file:///etc/passwd",
	} {
		if _, err := downloadRemoteMedia(t.Context(), rawURL, "image/png"); !errors.Is(err, errOutboundBlocked) {
			t.Errorf("downloadRemoteMedia(%q) = %v, want errOutboundBlocked", rawURL, err)
		}
	}
	if hits.Load() != 0 {
		t.Fatalf("expected 0 hits on local victim server, got %d", hits.Load())
	}

	// Verify remote media validator rejects redirect to metadata or non-http scheme.
	rmValidator := outboundValidator{
		kind:             "remote_media",
		requireAllowlist: false,
		allowRedirects:   true,
		maxRedirects:     10,
		lookupIP: func(ctx context.Context, host string) ([]netip.Addr, error) {
			return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
		},
	}
	for _, loc := range []string{"http://169.254.169.254/latest/meta-data/", "file:///etc/passwd"} {
		base := roundTripFunc(func(req *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusFound,
				Header:     http.Header{"Location": []string{loc}},
				Body:       io.NopCloser(bytes.NewReader(nil)),
				Request:    req,
			}, nil
		})
		client := rmValidator.newHTTPClientWithBase(0, base, nil)
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://cdn.example.com/a.png", nil)
		resp, err := client.Do(req)
		if resp != nil {
			resp.Body.Close()
		}
		if !errors.Is(err, errOutboundBlocked) {
			t.Errorf("remote media redirect to %q = %v, want errOutboundBlocked", loc, err)
		}
	}
}

type stubService struct{}

func (*stubService) State() string { return "RUNNING" }
func (*stubService) Models(context.Context) ([]aistudio.Model, error) {
	return []aistudio.Model{{ID: "gemini-3.8-flash", Name: "Gemini 3.8 Flash"}}, nil
}
func (*stubService) Generate(context.Context, aistudio.GenerateRequest) (<-chan aistudio.Event, error) {
	return nil, errors.New("unexpected upstream Generate call")
}
func (*stubService) CountTokens(context.Context, aistudio.TokenCountRequest) (aistudio.TokenCount, error) {
	return aistudio.TokenCount{}, errors.New("unexpected upstream CountTokens call")
}
