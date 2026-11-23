package cpa_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/api"
	"github.com/Mag1cFall/AIStudio2API/internal/camoufoxnative"
	"github.com/Mag1cFall/AIStudio2API/internal/cpa"
	"github.com/Mag1cFall/AIStudio2API/internal/pluginabi"
	"github.com/Mag1cFall/AIStudio2API/internal/pluginapi"
	"github.com/Mag1cFall/AIStudio2API/internal/waa"
)

func TestPluginLifecycleAndCapabilities(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	var hostCallsMu sync.Mutex
	var hostCalls []string
	p := cpa.NewPlugin(func(method string, payload []byte) ([]byte, error) {
		hostCallsMu.Lock()
		hostCalls = append(hostCalls, method)
		hostCallsMu.Unlock()
		return json.Marshal(pluginabi.Envelope{
			OK:     true,
			Result: json.RawMessage(`{}`),
		})
	})
	defer func() {
		_, _ = p.HandleMethod(pluginabi.MethodPluginShutdown, nil)
	}()

	cfgYAML := []byte("data_dir: " + tmpDir + "\nwaa_backend: spidermonkey\nauto_start: false\nsync_auth_files: false\n")
	regReq, _ := json.Marshal(map[string]any{
		"config_yaml":    cfgYAML,
		"schema_version": pluginabi.SchemaVersion,
	})

	// 1. plugin.register
	regRes, err := p.HandleMethod(pluginabi.MethodPluginRegister, regReq)
	if err != nil {
		t.Fatalf("plugin.register failed: %v", err)
	}
	regJSON, _ := json.Marshal(regRes)
	if !strings.Contains(string(regJSON), `"Name":"aistudio2api"`) {
		t.Fatalf("unexpected registration metadata: %s", string(regJSON))
	}
	if !strings.Contains(string(regJSON), `"Logo":"https://www.gstatic.com/lamda/images/gemini_sparkle_v002_d4735304ff6292a690345.svg"`) {
		t.Fatalf("missing expected logo in registration metadata: %s", string(regJSON))
	}
	if !strings.Contains(string(regJSON), `"executor":true`) || !strings.Contains(string(regJSON), `"auth_provider":true`) {
		t.Fatalf("missing expected capabilities: %s", string(regJSON))
	}
	if !strings.Contains(string(regJSON), `"request_lifecycle_plugin":false`) {
		t.Fatalf("expected request_lifecycle_plugin=false to avoid per-request no-op CGO RPC: %s", string(regJSON))
	}

	// 2. auth.identifier & auth.parse
	authIDRes, err := p.HandleMethod(pluginabi.MethodAuthIdentifier, nil)
	if err != nil {
		t.Fatalf("auth.identifier failed: %v", err)
	}
	authIDJSON, _ := json.Marshal(authIDRes)
	if !strings.Contains(string(authIDJSON), `"identifiers":["aistudio2api"]`) || strings.Contains(string(authIDJSON), `"identifiers":["aistudio2api","aistudio"`) {
		t.Fatalf("expected single primary identifier in auth.identifier, got: %s", string(authIDJSON))
	}

	// Unrelated provider should return Handled: false
	unrelatedParseReq, _ := json.Marshal(pluginapi.AuthParseRequest{
		Provider: "openai",
		FileName: "openai-key.json",
		RawJSON:  []byte(`{"type":"openai"}`),
	})
	unrelatedRes, err := p.HandleMethod(pluginabi.MethodAuthParse, unrelatedParseReq)
	if err != nil {
		t.Fatalf("auth.parse unrelated failed: %v", err)
	}
	if unrelatedRes.(pluginapi.AuthParseResponse).Handled {
		t.Fatalf("expected Handled=false for unrelated provider")
	}

	// Matching aistudio2api auth file
	matchingParseReq, _ := json.Marshal(pluginapi.AuthParseRequest{
		Provider: "aistudio2api",
		FileName: "aistudio2api-user@example.com.json",
		RawJSON:  []byte(`{"type":"aistudio2api","account_id":"user@example.com","tier":"PRO"}`),
	})
	matchingRes, err := p.HandleMethod(pluginabi.MethodAuthParse, matchingParseReq)
	if err != nil {
		t.Fatalf("auth.parse matching failed: %v", err)
	}
	parsedAuth := matchingRes.(pluginapi.AuthParseResponse)
	if !parsedAuth.Handled || parsedAuth.Auth.Provider != "aistudio2api" || parsedAuth.Auth.Attributes["account_id"] != "user@example.com" {
		t.Fatalf("unexpected parsed auth: %+v", parsedAuth)
	}

	// 3. model.static & model.for_auth & model.route
	staticRes, err := p.HandleMethod(pluginabi.MethodModelStatic, []byte(`{}`))
	if err != nil {
		t.Fatalf("model.static failed: %v", err)
	}
	modelResp := staticRes.(pluginapi.ModelResponse)
	if modelResp.Provider != "aistudio2api" || len(modelResp.Models) == 0 {
		t.Fatalf("expected non-empty models from model.static, got: %+v", modelResp)
	}
	hasPrefixed := false
	hasUnprefixed := false
	for _, m := range modelResp.Models {
		if m.ID == "gemini-3.1-pro-preview" {
			hasUnprefixed = true
		}
		if strings.HasPrefix(m.ID, "aistudio/") {
			hasPrefixed = true
		}
	}
	if !hasUnprefixed || hasPrefixed {
		t.Fatalf("expected unprefixed gemini-3.1-pro-preview without duplicate aistudio/* prefix in catalog")
	}

	routeReq, _ := json.Marshal(pluginapi.ModelRouteRequest{
		RequestedModel: "aistudio/gemini-3.1-pro-preview",
		Body:           []byte(`{"model":"aistudio/gemini-3.1-pro-preview","messages":[{"role":"user","content":"hi"}]}`),
	})
	routeRes, err := p.HandleMethod(pluginabi.MethodModelRoute, routeReq)
	if err != nil {
		t.Fatalf("model.route failed: %v", err)
	}
	rResp := routeRes.(pluginapi.ModelRouteResponse)
	if !rResp.Handled || rResp.TargetKind != pluginapi.ModelRouteTargetSelf || rResp.TargetModel != "gemini-3.1-pro-preview" {
		t.Fatalf("unexpected model.route response: %+v", rResp)
	}

	// 4. scheduler.pick
	schedReq, _ := json.Marshal(pluginapi.SchedulerPickRequest{
		Provider: "aistudio2api",
		Model:    "gemini-3.1-pro-preview",
		Candidates: []pluginapi.SchedulerAuthCandidate{
			{
				ID:         "aistudio2api-user@example.com.json",
				Provider:   "aistudio2api",
				Status:     "active",
				Attributes: map[string]string{"provider": "aistudio2api", "account_id": "user@example.com"},
			},
		},
	})
	schedRes, err := p.HandleMethod(pluginabi.MethodSchedulerPick, schedReq)
	if err != nil {
		t.Fatalf("scheduler.pick failed: %v", err)
	}
	sResp := schedRes.(pluginapi.SchedulerPickResponse)
	if !sResp.Handled {
		t.Fatalf("expected scheduler.pick Handled=true, got: %+v", sResp)
	}

	// 5. quota.describe & quota.fetch
	qDesc, err := p.HandleMethod(pluginabi.MethodQuotaDescribe, nil)
	if err != nil {
		t.Fatalf("quota.describe failed: %v", err)
	}
	if !qDesc.(pluginapi.QuotaDescribeResponse).SupportsReset {
		t.Fatalf("expected SupportsReset=true")
	}

	qFetch, err := p.HandleMethod(pluginabi.MethodQuotaFetch, []byte(`{"provider":"aistudio2api"}`))
	if err != nil {
		t.Fatalf("quota.fetch failed: %v", err)
	}
	if qFetch.(pluginapi.QuotaFetchResponse).Subscription == nil {
		t.Fatalf("expected Subscription in quota.fetch")
	}

	// 6. management.register & management.handle (Web UI & Status API)
	mgmtReg, err := p.HandleMethod(pluginabi.MethodManagementRegister, nil)
	if err != nil {
		t.Fatalf("management.register failed: %v", err)
	}
	mgmtRegJSON, _ := json.Marshal(mgmtReg)
	if !strings.Contains(string(mgmtRegJSON), `"/accounts"`) {
		t.Fatalf("missing resource route in management.register: %s", string(mgmtRegJSON))
	}

	uiReq, _ := json.Marshal(pluginapi.ManagementRequest{
		Method: http.MethodGet,
		Path:   "/v0/resource/plugins/aistudio2api/accounts",
	})
	uiRes, err := p.HandleMethod(pluginabi.MethodManagementHandle, uiReq)
	if err != nil {
		t.Fatalf("management.handle UI failed: %v", err)
	}
	uiResp := uiRes.(pluginapi.ManagementResponse)
	if uiResp.StatusCode != http.StatusOK || !strings.Contains(string(uiResp.Body), "启动本机 Chrome 无痕登录") {
		t.Fatalf("unexpected UI response: status=%d len=%d", uiResp.StatusCode, len(uiResp.Body))
	}
	if strings.Contains(string(uiResp.Body), "autologin") || strings.Contains(string(uiResp.Body), "URLSearchParams(window.location.search)") {
		t.Fatalf("resource UI page must not auto-trigger actions or read credentials from URL query parameters")
	}

	resStatusReq, _ := json.Marshal(pluginapi.ManagementRequest{
		Method: http.MethodGet,
		Path:   "/v0/resource/plugins/aistudio2api/accounts",
		Query:  map[string][]string{"view": {"status"}},
	})
	resStatusRes, err := p.HandleMethod(pluginabi.MethodManagementHandle, resStatusReq)
	if err != nil {
		t.Fatalf("management.handle resource with query failed: %v", err)
	}
	resStatusResp := resStatusRes.(pluginapi.ManagementResponse)
	if resStatusResp.StatusCode != http.StatusOK || !strings.Contains(resStatusResp.Headers.Get("Content-Type"), "text/html") || strings.Contains(string(resStatusResp.Body), `"waa_backend":"spidermonkey"`) {
		t.Fatalf("expected static HTML only on /v0/resource/plugins/..., got content-type=%q", resStatusResp.Headers.Get("Content-Type"))
	}
	statusReq, _ := json.Marshal(pluginapi.ManagementRequest{
		Method: http.MethodGet,
		Path:   "/v0/management/aistudio2api/status",
	})
	statusRes, err := p.HandleMethod(pluginabi.MethodManagementHandle, statusReq)
	if err != nil {
		t.Fatalf("management.handle status failed: %v", err)
	}
	statusResp := statusRes.(pluginapi.ManagementResponse)
	if statusResp.StatusCode != http.StatusOK || !strings.Contains(string(statusResp.Body), `"waa_backend":"spidermonkey"`) || !strings.Contains(string(statusResp.Body), `"chrome_available"`) {
		t.Fatalf("unexpected status response: %s", string(statusResp.Body))
	}

	t.Setenv("AISTUDIO2API_NO_AUTO_BROWSER", "1")
	startRes, err := p.HandleMethod(pluginabi.MethodAuthLoginStart, []byte(`{"provider":"aistudio2api","base_url":"http://127.0.0.1:8317/v8/management/oauth/callback"}`))
	if err != nil {
		t.Fatalf("auth.login_start failed: %v", err)
	}
	startResp := startRes.(pluginapi.AuthLoginStartResponse)
	if startResp.URL != "http://127.0.0.1:8317/v0/resource/plugins/aistudio2api/accounts" {
		t.Fatalf("expected valid full URL in AuthLoginStartResponse, got %q", startResp.URL)
	}
}

func TestAuthFileDisabledSyncOnServiceStopAndShutdown(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	t.Setenv("USERPROFILE", tmpHome)
	authDir := filepath.Join(tmpHome, ".cli-proxy-api")
	if err := os.MkdirAll(authDir, 0o700); err != nil {
		t.Fatalf("mkdir authDir: %v", err)
	}
	dataDir := filepath.Join(tmpHome, ".aistudio2api")
	authFilePath := filepath.Join(authDir, "aistudio2api-testuser@gmail.com.json")
	initialPayload := `{
		"type": "aistudio2api",
		"provider": "aistudio2api",
		"account_id": "testuser@gmail.com",
		"email": "testuser@gmail.com",
		"label": "testuser@gmail.com",
		"disabled": true,
		"storage_state": {
			"cookies": [
				{"name": "SAPISID", "value": "test-sapisid-val", "domain": ".google.com", "path": "/", "secure": true, "sameSite": "None"},
				{"name": "__Secure-1PSID", "value": "test-1psid-val", "domain": ".google.com", "path": "/", "secure": true, "sameSite": "None"},
				{"name": "__Secure-3PSID", "value": "test-3psid-val", "domain": ".google.com", "path": "/", "secure": true, "sameSite": "None"},
				{"name": "SID", "value": "test-sid-val", "domain": ".google.com", "path": "/", "sameSite": "Lax"}
			],
			"origins": []
		}
	}`
	if err := os.WriteFile(authFilePath, []byte(initialPayload), 0o600); err != nil {
		t.Fatalf("write initial auth file: %v", err)
	}

	p := cpa.NewPlugin(nil)
	defer func() {
		_, _ = p.HandleMethod(pluginabi.MethodPluginShutdown, []byte(`{}`))
	}()

	cfgYAML := []byte("data_dir: \"" + filepath.ToSlash(dataDir) + "\"\nwaa_backend: spidermonkey\nauto_start: false\nsync_auth_files: true\n")
	regReq, _ := json.Marshal(map[string]any{
		"config_yaml":    cfgYAML,
		"schema_version": pluginabi.SchemaVersion,
	})
	if _, err := p.HandleMethod(pluginabi.MethodPluginRegister, regReq); err != nil {
		t.Fatalf("plugin.register failed: %v", err)
	}
	// Simulate auto_start=true runtime state for startup sync test
	cfgYAMLAuto := []byte("data_dir: \"" + filepath.ToSlash(dataDir) + "\"\nwaa_backend: spidermonkey\nauto_start: true\nsync_auth_files: true\n")
	regReqAuto, _ := json.Marshal(map[string]any{
		"config_yaml":    cfgYAMLAuto,
		"schema_version": pluginabi.SchemaVersion,
	})
	if _, err := p.HandleMethod(pluginabi.MethodPluginReconfigure, regReqAuto); err != nil {
		t.Fatalf("plugin.reconfigure failed: %v", err)
	}

	// 1. Parse auth during startup window -> should import account as enabled and flip disabled=false on disk
	parseReq, _ := json.Marshal(pluginapi.AuthParseRequest{
		FileName: "aistudio2api-testuser@gmail.com.json",
		RawJSON:  []byte(initialPayload),
	})
	parseRes, err := p.HandleMethod(pluginabi.MethodAuthParse, parseReq)
	if err != nil {
		t.Fatalf("auth.parse failed: %v", err)
	}
	parseResp := parseRes.(pluginapi.AuthParseResponse)
	if !parseResp.Handled || parseResp.Auth.Disabled {
		t.Fatalf("expected auth to be enabled on plugin startup, got Disabled=%v", parseResp.Auth.Disabled)
	}

	checkDiskDisabled := func(wantDisabled bool, wantStopped bool, step string) {
		t.Helper()
		raw, err := os.ReadFile(authFilePath)
		if err != nil {
			t.Fatalf("%s: read auth file: %v", step, err)
		}
		var doc map[string]any
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Fatalf("%s: unmarshal auth file: %v", step, err)
		}
		gotDisabled, _ := doc["disabled"].(bool)
		gotStopped, _ := doc["service_stopped"].(bool)
		if gotDisabled != wantDisabled || gotStopped != wantStopped {
			t.Fatalf("%s: disk auth file disabled=%v service_stopped=%v, want disabled=%v service_stopped=%v", step, gotDisabled, gotStopped, wantDisabled, wantStopped)
		}
	}
	checkDiskDisabled(false, false, "after startup auth.parse")

	// 2. Stop service via management API -> should mark disk auth file disabled=true, service_stopped=true
	stopReq, _ := json.Marshal(pluginapi.ManagementRequest{
		Method: http.MethodPost,
		Path:   "/v0/management/aistudio2api/service/stop",
	})
	if _, err := p.HandleMethod(pluginabi.MethodManagementHandle, stopReq); err != nil {
		t.Fatalf("service/stop failed: %v", err)
	}
	checkDiskDisabled(true, true, "after service/stop")

	// 3. Re-enable plugin (reconfigure with auto_start=true) -> should restore disabled=false, service_stopped=false
	if _, err := p.HandleMethod(pluginabi.MethodPluginReconfigure, regReqAuto); err != nil {
		t.Fatalf("plugin.reconfigure failed: %v", err)
	}
	checkDiskDisabled(false, false, "after plugin reconfigure/enable")

	// 4. Quiesce/Shutdown plugin -> should mark disk auth file disabled=true, service_stopped=true
	if _, err := p.HandleMethod(pluginabi.MethodPluginQuiesce, []byte(`{}`)); err != nil {
		t.Fatalf("plugin.quiesce failed: %v", err)
	}
	checkDiskDisabled(true, true, "after plugin.quiesce")
}

func TestSpiderMonkeyBotGuardVMIntegration(t *testing.T) {
	// Verify that the embedded SpiderMonkey C++ engine initializes the Firefox DOM, loads a BotGuard VM, and produces a WAA proof snapshot
	rt, err := waa.NewRuntime(t.Context(), waa.Options{
		Challenge: waa.Challenge{
			InterpreterHash: "test_hash",
			Program:         "test_program_bytes",
			GlobalName:      "_bg_test",
		},
		Interpreter: `window._bg_test = { a: function(prog, cb) { cb(function(deliver) { deliver("spidermonkey_waa_proof_" + prog); }, function(){}); } };`,
	})
	if err != nil {
		t.Fatalf("waa.NewRuntime failed: %v", err)
	}
	defer rt.Close()

	proof, err := rt.Proof(t.Context(), "sha256_digest", "hello world")
	if err != nil {
		t.Fatalf("rt.Snapshot failed: %v", err)
	}
	if proof != "spidermonkey_waa_proof_test_program_bytes" {
		t.Fatalf("unexpected WAA proof from SpiderMonkey: %q", proof)
	}
}

func TestBuiltDynamicLibraryExists(t *testing.T) {
	dylibPath := strings.TrimSpace(os.Getenv("CPA_PLUGIN_LIB_PATH"))
	if dylibPath == "" {
		ext := ".so"
		switch runtime.GOOS {
		case "darwin":
			ext = ".dylib"
		case "windows":
			ext = ".dll"
		}
		dylibPath = filepath.Join("..", "..", "dist", "plugins", runtime.GOOS, runtime.GOARCH, "aistudio2api"+ext)
	}
	info, err := os.Stat(dylibPath)
	if err != nil {
		t.Skipf("shared library not found at %s: %v", dylibPath, err)
	}
	if info.Size() < 10*1024*1024 {
		t.Fatalf("expected static-linked SpiderMonkey shared library > 10MB, got %d bytes", info.Size())
	}
}

func TestDefaultUpstreamChannelsAndStreamedCallUnaryFallback(t *testing.T) {
	_, appCfg, err := cpa.ParsePluginConfig([]byte(`{}`))
	if err != nil {
		t.Fatalf("ParsePluginConfig failed: %v", err)
	}
	if len(appCfg.UpstreamChannels) != 1 || appCfg.UpstreamChannels[0] != "playground" {
		t.Fatalf("expected default UpstreamChannels=[\"playground\"], got %#v", appCfg.UpstreamChannels)
	}

	// Verify BuildGenerationDisabled detects PROXY_GENERATION_DISABLED errors for unary fallback
	disabledErr := &aistudio.RPCError{
		Method:     "ProxyStreamedCall",
		StatusCode: http.StatusBadRequest,
		Code:       3,
		Message:    "PROXY_GENERATION_DISABLED: streamGenerateContent is not supported",
	}
	if !aistudio.BuildGenerationDisabled(disabledErr) {
		t.Fatalf("expected BuildGenerationDisabled=true for %v", disabledErr)
	}

	// Verify embedded webui.html serves cleanly with the account lock conflict diagnostic
	p := cpa.NewPlugin(nil)
	defer func() {
		_, _ = p.HandleMethod(pluginabi.MethodPluginShutdown, nil)
	}()
	uiReq, _ := json.Marshal(pluginapi.ManagementRequest{
		Method: http.MethodGet,
		Path:   "/v0/resource/plugins/aistudio2api/accounts",
	})
	uiRes, err := p.HandleMethod(pluginabi.MethodManagementHandle, uiReq)
	if err != nil {
		t.Fatalf("webui management handle failed: %v", err)
	}
	uiResp := uiRes.(pluginapi.ManagementResponse)
	if uiResp.StatusCode != http.StatusOK || !strings.Contains(string(uiResp.Body), "进程锁冲突 (多端共用)") {
		t.Fatalf("unexpected webui response status=%d len=%d", uiResp.StatusCode, len(uiResp.Body))
	}
}

func TestExcludedModelsAndAliasPreservation(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	_ = os.MkdirAll(filepath.Join(tmpDir, ".cli-proxy-api"), 0o700)
	accDir := filepath.Join(tmpDir, "accounts", "test@example.com")
	_ = os.MkdirAll(accDir, 0o700)
	_ = os.WriteFile(filepath.Join(accDir, "account.json"), []byte(`{"label":"test@example.com","enabled":true,"locale":"en-US","timezone":"Asia/Tokyo"}`), 0o600)
	_ = os.WriteFile(filepath.Join(accDir, "storage-state.json"), []byte(`{"cookies":[{"name":"SAPISID","value":"123","domain":".google.com","path":"/","expires":-1},{"name":"__Secure-1PSID","value":"456","domain":".google.com","path":"/","expires":-1}]}`), 0o600)
	p := cpa.NewPlugin(nil)
	defer func() {
		_, _ = p.HandleMethod(pluginabi.MethodPluginShutdown, nil)
	}()

	cfgYAML := []byte("data_dir: " + tmpDir + "\nwaa_backend: spidermonkey\nauto_start: false\nsync_auth_files: true\nmodel_mapping: my-custom-pro=gemini-3.1-pro-preview\n")
	regReq, _ := json.Marshal(map[string]any{
		"config_yaml":    cfgYAML,
		"schema_version": pluginabi.SchemaVersion,
	})
	regRes, err := p.HandleMethod(pluginabi.MethodPluginRegister, regReq)
	if err != nil {
		t.Fatalf("plugin.register failed: %v", err)
	}
	regJSON, _ := json.Marshal(regRes)
	if !strings.Contains(string(regJSON), `"executor_model_scope":"oauth"`) {
		t.Fatalf("expected executor_model_scope=oauth, got %s", string(regJSON))
	}

	rawCred := []byte(`{
		"type": "aistudio2api",
		"account_id": "test@example.com",
		"tier": "Pro",
		"prefix": "ais",
		"priority": 10,
		"weight": 5,
		"note": "work account",
		"excluded_models": ["gemini-3.8-flash*"],
		"model_aliases": {"fast": "gemini-3.1-pro-preview"}
	}`)
	parseReq, _ := json.Marshal(pluginapi.AuthParseRequest{
		Provider: "aistudio2api",
		FileName: "aistudio2api-test@example.com.json",
		RawJSON:  rawCred,
	})
	parseRes, err := p.HandleMethod(pluginabi.MethodAuthParse, parseReq)
	if err != nil {
		t.Fatalf("auth.parse failed: %v", err)
	}
	parsed := parseRes.(pluginapi.AuthParseResponse)
	if !parsed.Handled || parsed.Auth.Prefix != "ais" {
		t.Fatalf("expected Handled=true and Prefix=ais, got %+v", parsed)
	}
	if parsed.Auth.Metadata["note"] != "work account" {
		t.Fatalf("expected Metadata[note] preserved, got %#v", parsed.Auth.Metadata)
	}
	if !strings.Contains(string(parsed.Auth.StorageJSON), `"excluded_models"`) || !strings.Contains(string(parsed.Auth.StorageJSON), `"model_aliases"`) {
		t.Fatalf("expected StorageJSON to preserve excluded_models and model_aliases, got %s", string(parsed.Auth.StorageJSON))
	}

	// Mapped alias should appear in catalog and route to target model
	routeAliasReq, _ := json.Marshal(pluginapi.ModelRouteRequest{
		RequestedModel: "my-custom-pro",
	})
	routeAliasRes, err := p.HandleMethod(pluginabi.MethodModelRoute, routeAliasReq)
	if err != nil {
		t.Fatalf("model.route alias failed: %v", err)
	}
	aliasRoute := routeAliasRes.(pluginapi.ModelRouteResponse)
	if !aliasRoute.Handled || aliasRoute.TargetModel != "gemini-3.1-pro-preview" {
		t.Fatalf("expected my-custom-pro routed to gemini-3.1-pro-preview, got %+v", aliasRoute)
	}

	// Excluded model on account should not be hijacked by model.route
	routeExclReq, _ := json.Marshal(pluginapi.ModelRouteRequest{
		RequestedModel: "gemini-3.8-flash",
	})
	routeExclRes, err := p.HandleMethod(pluginabi.MethodModelRoute, routeExclReq)
	if err != nil {
		t.Fatalf("model.route excluded failed: %v", err)
	}
	if routeExclRes.(pluginapi.ModelRouteResponse).Handled {
		t.Fatalf("expected Handled=false for excluded model gemini-3.8-flash")
	}

	// Re-register/reconfigure with existing account in runtime must not deadlock
	reconfRes, err := p.HandleMethod(pluginabi.MethodPluginReconfigure, regReq)
	if err != nil || reconfRes == nil {
		t.Fatalf("plugin.reconfigure with existing account failed: %v", err)
	}
}

func TestProxyPasswordPreservationExecutorGuardAndCustomAuthDir(t *testing.T) {
	tmpDir := t.TempDir()
	dataDir := filepath.Join(tmpDir, "data")
	customAuthDir := filepath.Join(tmpDir, "custom-host-auths")
	if err := os.MkdirAll(customAuthDir, 0o700); err != nil {
		t.Fatalf("mkdir custom auth dir: %v", err)
	}
	accDir := filepath.Join(dataDir, "accounts", "proxyuser@gmail.com")
	if err := os.MkdirAll(accDir, 0o700); err != nil {
		t.Fatalf("mkdir account dir: %v", err)
	}
	_ = os.WriteFile(filepath.Join(accDir, "account.json"), []byte(`{"label":"proxyuser@gmail.com","enabled":true,"proxy":"http://alice:super-secret-pass@127.0.0.1:7890","locale":"en-US","timezone":"America/Los_Angeles"}`), 0o600)
	_ = os.WriteFile(filepath.Join(accDir, "storage-state.json"), []byte(`{"cookies":[{"name":"SAPISID","value":"sapisid-val","domain":".google.com","path":"/","secure":true,"sameSite":"None","expires":-1},{"name":"__Secure-1PSID","value":"1psid-val","domain":".google.com","path":"/","secure":true,"sameSite":"None","expires":-1},{"name":"__Secure-3PSID","value":"3psid-val","domain":".google.com","path":"/","secure":true,"sameSite":"None","expires":-1},{"name":"SID","value":"sid-val","domain":".google.com","path":"/","sameSite":"Lax","expires":-1}],"origins":[]}`), 0o600)

	p := cpa.NewPlugin(nil)
	defer func() {
		_, _ = p.HandleMethod(pluginabi.MethodPluginShutdown, []byte(`{}`))
	}()

	cfgYAML := []byte("data_dir: \"" + filepath.ToSlash(dataDir) + "\"\nwaa_backend: spidermonkey\nauto_start: false\nsync_auth_files: true\n")
	regReq, _ := json.Marshal(map[string]any{
		"config_yaml":    cfgYAML,
		"schema_version": pluginabi.SchemaVersion,
	})
	if _, err := p.HandleMethod(pluginabi.MethodPluginRegister, regReq); err != nil {
		t.Fatalf("plugin.register failed: %v", err)
	}

	// 1. Report custom Host.AuthDir via static models call
	staticReq, _ := json.Marshal(pluginapi.StaticModelRequest{
		Host: pluginapi.HostConfigSummary{AuthDir: customAuthDir},
	})
	if _, err := p.HandleMethod(pluginabi.MethodModelStatic, staticReq); err != nil {
		t.Fatalf("model.static failed: %v", err)
	}

	// 2. Verify GET /accounts redacts proxy password in response
	listReq, _ := json.Marshal(pluginapi.ManagementRequest{
		Method: http.MethodGet,
		Path:   "/v0/management/aistudio2api/accounts",
	})
	listRes, err := p.HandleMethod(pluginabi.MethodManagementHandle, listReq)
	if err != nil {
		t.Fatalf("management list accounts failed: %v", err)
	}
	listMgmt := listRes.(pluginapi.ManagementResponse)
	if strings.Contains(string(listMgmt.Body), "super-secret-pass") {
		t.Fatalf("GET /accounts leaked cleartext proxy password: %s", string(listMgmt.Body))
	}

	// Sync to custom Host.AuthDir and verify real proxy password is saved on disk
	syncReq, _ := json.Marshal(pluginapi.ManagementRequest{
		Method: http.MethodPost,
		Path:   "/v0/management/aistudio2api/accounts/sync-auth",
	})
	if _, err := p.HandleMethod(pluginabi.MethodManagementHandle, syncReq); err != nil {
		t.Fatalf("sync-auth failed: %v", err)
	}
	customAuthFile := filepath.Join(customAuthDir, "aistudio2api-proxyuser@gmail.com.json")
	rawAuth, err := os.ReadFile(customAuthFile)
	if err != nil {
		t.Fatalf("expected auth file in custom Host.AuthDir %s: %v", customAuthFile, err)
	}
	if !strings.Contains(string(rawAuth), "http://alice:super-secret-pass@127.0.0.1:7890") {
		t.Fatalf("expected custom auth file to retain real proxy password, got: %s", string(rawAuth))
	}
	// 3. Toggle account enabled without sending proxy (and then with redacted xxxxx proxy) -> must preserve real password
	for _, payload := range []map[string]any{
		{"account_id": "proxyuser@gmail.com", "enabled": false},
		{"account_id": "proxyuser@gmail.com", "enabled": false, "proxy": "http://alice:xxxxx@127.0.0.1:7890"},
	} {
		updBody, _ := json.Marshal(payload)
		updReq, _ := json.Marshal(pluginapi.ManagementRequest{
			Method: http.MethodPost,
			Path:   "/v0/management/aistudio2api/accounts/update",
			Body:   updBody,
		})
		updRes, uErr := p.HandleMethod(pluginabi.MethodManagementHandle, updReq)
		if uErr != nil {
			t.Fatalf("management update failed: %v", uErr)
		}
		updMgmt := updRes.(pluginapi.ManagementResponse)
		if updMgmt.StatusCode != http.StatusOK {
			t.Fatalf("expected 200 OK on update, got %d: %s", updMgmt.StatusCode, string(updMgmt.Body))
		}
		if strings.Contains(string(updMgmt.Body), "super-secret-pass") {
			t.Fatalf("management update response leaked cleartext proxy password: %s", string(updMgmt.Body))
		}
	}

	rt, err := p.Runtime()
	if err != nil {
		t.Fatalf("p.Runtime() failed: %v", err)
	}
	accs, err := rt.Admin.Accounts(t.Context())
	if err != nil || len(accs) != 1 {
		t.Fatalf("expected 1 account, got %d (err=%v)", len(accs), err)
	}
	if accs[0].Proxy != "http://alice:super-secret-pass@127.0.0.1:7890" {
		t.Fatalf("real proxy password was clobbered: got %q", accs[0].Proxy)
	}

	// 4. Verify /api/* routes are blocked on executor.http_request
	execHTTPReq, _ := json.Marshal(pluginapi.ExecutorHTTPRequest{
		Method:      http.MethodGet,
		URL:         "/api/accounts",
		StorageJSON: rawAuth,
	})
	execHTTPRes, err := p.HandleMethod(pluginabi.MethodExecutorHTTPRequest, execHTTPReq)
	if err != nil {
		t.Fatalf("executor.http_request returned unexpected error: %v", err)
	}
	if execHTTPRes.(pluginapi.ExecutorHTTPResponse).StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 Forbidden for /api/accounts on executor.http_request, got %d", execHTTPRes.(pluginapi.ExecutorHTTPResponse).StatusCode)
	}

	// 5. Verify live reload updates model_aliases and routing_strategy on existing runtime
	reloadYAML := []byte("data_dir: \"" + filepath.ToSlash(dataDir) + "\"\nwaa_backend: spidermonkey\nauto_start: false\nsync_auth_files: true\nrouting_strategy: fill-first\nmodel_aliases: \"live-alias=gemini-3.8-flash\"\n")
	reloadReq, _ := json.Marshal(map[string]any{
		"config_yaml":    reloadYAML,
		"schema_version": pluginabi.SchemaVersion,
	})
	if _, err := p.HandleMethod(pluginabi.MethodPluginReconfigure, reloadReq); err != nil {
		t.Fatalf("plugin.reconfigure failed: %v", err)
	}
	routeLiveReq, _ := json.Marshal(pluginapi.ModelRouteRequest{RequestedModel: "live-alias"})
	routeLiveRes, err := p.HandleMethod(pluginabi.MethodModelRoute, routeLiveReq)
	if err != nil {
		t.Fatalf("model.route live-alias failed: %v", err)
	}
	liveRoute := routeLiveRes.(pluginapi.ModelRouteResponse)
	if !liveRoute.Handled || liveRoute.TargetModel != "gemini-3.8-flash" {
		t.Fatalf("expected live-alias routed to gemini-3.8-flash after reconfigure, got %+v", liveRoute)
	}
}

func TestPrefixedModelRouteUsesProviderWhenAvailableAndApplyLiveConfigSyncsHedgeAndCapacity(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	t.Setenv("USERPROFILE", tmpDir)
	t.Setenv("AISTUDIO2API_DATA_DIR", tmpDir)

	p := cpa.NewPlugin(nil)
	defer func() {
		_, _ = p.HandleMethod(pluginabi.MethodPluginShutdown, nil)
	}()

	// 1. Calls p.Runtime() BEFORE plugin.register / plugin.reconfigure
	rt, err := p.Runtime()
	if err != nil {
		t.Fatalf("p.Runtime() before register/reconfigure failed: %v", err)
	}

	initialAuto, initialAfter, _ := rt.HedgeSettings()
	if !initialAuto || initialAfter != 0 {
		t.Fatalf("expected initial default hedgeAuto=true, hedgeAfter=0, got auto=%v after=%v", initialAuto, initialAfter)
	}
	if initialTarget := rt.WarmWorkerTarget(); initialTarget != 2 {
		t.Fatalf("expected initial default WarmWorkerTarget=2, got %d", initialTarget)
	}

	// 2. Calls plugin.reconfigure with updated hedge (including duration suffix) and capacity settings
	reconfYAML := []byte("data_dir: \"" + filepath.ToSlash(tmpDir) + "\"\nwaa_backend: spidermonkey\nauto_start: false\nmodel_prefix: \"AIStudio/\"\nfirst_event_hedge: \"6s\"\nfirst_event_hedge_models: \"gemini-4-argon,gemini-3.8-flash\"\nwarm_workers: 7\nmax_active_workers: 7\nper_account_concurrency: 3\n")
	reconfReq, _ := json.Marshal(map[string]any{
		"config_yaml":    reconfYAML,
		"schema_version": pluginabi.SchemaVersion,
	})
	if _, err := p.HandleMethod(pluginabi.MethodPluginReconfigure, reconfReq); err != nil {
		t.Fatalf("plugin.reconfigure failed: %v", err)
	}

	// 3. Verifies rt.HedgeSettings() and rt.WarmWorkerTarget()
	hedgeAuto, hedgeAfter, hedgeModels := rt.HedgeSettings()
	if hedgeAuto {
		t.Fatalf("expected hedgeAuto=false after reconfigure, got true")
	}
	if hedgeAfter != 6*time.Second {
		t.Fatalf("expected hedgeAfter=6s after reconfigure, got %v", hedgeAfter)
	}
	hasArgon := false
	hasFlash := false
	for _, m := range hedgeModels {
		if m == "gemini-4-argon" {
			hasArgon = true
		}
		if m == "gemini-3.8-flash" {
			hasFlash = true
		}
	}
	if !hasArgon || !hasFlash {
		t.Fatalf("expected hedgeModels to contain gemini-4-argon and gemini-3.8-flash, got: %v", hedgeModels)
	}

	if target := rt.WarmWorkerTarget(); target != 7 {
		t.Fatalf("expected WarmWorkerTarget=7 after reconfigure, got %d", target)
	}

	// 4. Verifies model.route for "AIStudio/gemini-4-argon" with AvailableProviders: []string{"aistudio2api"}
	routeWithProviderReq, _ := json.Marshal(pluginapi.ModelRouteRequest{
		RequestedModel:     "AIStudio/gemini-4-argon",
		AvailableProviders: []string{"aistudio2api"},
		Body:               []byte(`{"model":"AIStudio/gemini-4-argon","messages":[{"role":"user","content":"hi"}]}`),
	})
	routeRes, err := p.HandleMethod(pluginabi.MethodModelRoute, routeWithProviderReq)
	if err != nil {
		t.Fatalf("model.route with provider failed: %v", err)
	}
	rResp, ok := routeRes.(pluginapi.ModelRouteResponse)
	if !ok {
		t.Fatalf("expected ModelRouteResponse, got %#v", routeRes)
	}
	if !rResp.Handled {
		t.Fatalf("expected model.route with provider Handled=true, got: %+v", rResp)
	}
	if rResp.TargetKind != pluginapi.ModelRouteTargetProvider {
		t.Fatalf("expected TargetKind=%v, got %v", pluginapi.ModelRouteTargetProvider, rResp.TargetKind)
	}
	if rResp.Target != "aistudio2api" {
		t.Fatalf("expected Target=aistudio2api, got %q", rResp.Target)
	}
	if rResp.TargetModel != "gemini-4-argon" {
		t.Fatalf("expected TargetModel=gemini-4-argon, got %q", rResp.TargetModel)
	}

	// 5. Verifies model.route for "AIStudio/gemini-4-argon" with AvailableProviders: nil
	routeWithoutProviderReq, _ := json.Marshal(pluginapi.ModelRouteRequest{
		RequestedModel:     "AIStudio/gemini-4-argon",
		AvailableProviders: nil,
		Body:               []byte(`{"model":"AIStudio/gemini-4-argon","messages":[{"role":"user","content":"hi"}]}`),
	})
	routeNoRes, err := p.HandleMethod(pluginabi.MethodModelRoute, routeWithoutProviderReq)
	if err != nil {
		t.Fatalf("model.route without provider failed: %v", err)
	}
	rNoResp, ok := routeNoRes.(pluginapi.ModelRouteResponse)
	if !ok {
		t.Fatalf("expected ModelRouteResponse, got %#v", routeNoRes)
	}
	if !rNoResp.Handled {
		t.Fatalf("expected model.route without provider Handled=true, got: %+v", rNoResp)
	}
	if rNoResp.TargetKind != pluginapi.ModelRouteTargetSelf {
		t.Fatalf("expected TargetKind=%v, got %v", pluginapi.ModelRouteTargetSelf, rNoResp.TargetKind)
	}
	if rNoResp.Target != "aistudio2api" {
		t.Fatalf("expected Target=aistudio2api, got %q", rNoResp.Target)
	}
	if rNoResp.TargetModel != "gemini-4-argon" {
		t.Fatalf("expected TargetModel=gemini-4-argon, got %q", rNoResp.TargetModel)
	}

	// 6. When host oauth.model-alias already defines alias "AIStudio/gemini-4-argon" -> "gemini-4-argon",
	// model.route returns Handled=false so the host matches the registered alias directly without 503.
	staticReqWithAlias, _ := json.Marshal(pluginapi.StaticModelRequest{
		Host: pluginapi.HostConfigSummary{
			OAuthModelAlias: map[string][]pluginapi.ModelAlias{
				"aistudio2api": {
					{Name: "gemini-4-argon", Alias: "AIStudio/gemini-4-argon"},
				},
			},
		},
	})
	if _, err := p.HandleMethod(pluginabi.MethodModelStatic, staticReqWithAlias); err != nil {
		t.Fatalf("model.static with OAuthModelAlias failed: %v", err)
	}
	routeHostAliasRes, err := p.HandleMethod(pluginabi.MethodModelRoute, routeWithProviderReq)
	if err != nil {
		t.Fatalf("model.route with host alias failed: %v", err)
	}
	rHostAliasResp, ok := routeHostAliasRes.(pluginapi.ModelRouteResponse)
	if !ok {
		t.Fatalf("expected ModelRouteResponse, got %#v", routeHostAliasRes)
	}
	if rHostAliasResp.Handled {
		t.Fatalf("expected Handled=false when host OAuthModelAlias already registers AIStudio/gemini-4-argon, got: %+v", rHostAliasResp)
	}
}

func TestSchedulerPickDefersToHostSessionAffinityWhenPartialPoolCooldown(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	t.Setenv("USERPROFILE", tmpDir)
	_ = os.MkdirAll(filepath.Join(tmpDir, ".cli-proxy-api"), 0o700)
	for _, email := range []string{"cooled@example.com", "healthy@example.com"} {
		accDir := filepath.Join(tmpDir, "accounts", email)
		_ = os.MkdirAll(accDir, 0o700)
		_ = os.WriteFile(filepath.Join(accDir, "account.json"), []byte(`{"label":"`+email+`","enabled":true,"locale":"en-US","timezone":"Asia/Tokyo"}`), 0o600)
		_ = os.WriteFile(filepath.Join(accDir, "storage-state.json"), []byte(`{"cookies":[{"name":"SAPISID","value":"123","domain":".google.com","path":"/","expires":-1},{"name":"__Secure-1PAPISID","value":"456","domain":".google.com","path":"/","expires":-1},{"name":"__Secure-3PAPISID","value":"789","domain":".google.com","path":"/","expires":-1}]}`), 0o600)
	}

	p := cpa.NewPlugin(nil)
	defer func() {
		_, _ = p.HandleMethod(pluginabi.MethodPluginShutdown, nil)
	}()

	cfgYAML := []byte("data_dir: \"" + filepath.ToSlash(tmpDir) + "\"\nwaa_backend: spidermonkey\nauto_start: false\nsync_auth_files: false\n")
	regReq, _ := json.Marshal(map[string]any{
		"config_yaml":    cfgYAML,
		"schema_version": pluginabi.SchemaVersion,
	})
	if _, err := p.HandleMethod(pluginabi.MethodPluginRegister, regReq); err != nil {
		t.Fatalf("plugin.register failed: %v", err)
	}

	rt, err := p.Runtime()
	if err != nil {
		t.Fatalf("runtime error: %v", err)
	}
	for _, email := range []string{"cooled@example.com", "healthy@example.com"} {
		if err := rt.Pool().SetCatalog(email, aistudio.BenefitTierPro, []aistudio.Model{
			{ID: "gemini-4-argon", Methods: []string{"generateContent"}, Capabilities: map[string]bool{"chat_model": true}},
		}); err != nil {
			t.Fatalf("SetCatalog %s failed: %v", email, err)
		}
	}

	cooldownScope := aistudio.ChannelCooldownScope(aistudio.ChannelPlayground, "gemini-4-argon")
	if err := rt.Pool().MarkCooldownIfGeneration("cooled@example.com", cooldownScope, 1, time.Now(), time.Now().Add(time.Hour), "quota_exhausted"); err != nil {
		t.Fatalf("mark cooldown failed: %v", err)
	}

	// Simulate host-dispatched scheduler.pick (with SelectionIDMetadataKey set) where
	// cooled@example.com is still passed by the host alongside healthy@example.com.
	schedReq, _ := json.Marshal(pluginapi.SchedulerPickRequest{
		Provider: "aistudio2api",
		Model:    "AIStudio/gemini-4-argon",
		Options: pluginapi.SchedulerOptions{
			Metadata: map[string]any{
				pluginapi.SelectionIDMetadataKey: "sel-123",
			},
		},
		Candidates: []pluginapi.SchedulerAuthCandidate{
			{
				ID:         "aistudio2api-cooled@example.com.json",
				Provider:   "aistudio2api",
				Status:     "active",
				Attributes: map[string]string{"provider": "aistudio2api", "account_id": "cooled@example.com"},
			},
			{
				ID:         "aistudio2api-healthy@example.com.json",
				Provider:   "aistudio2api",
				Status:     "active",
				Attributes: map[string]string{"provider": "aistudio2api", "account_id": "healthy@example.com"},
			},
		},
	})
	schedRes, err := p.HandleMethod(pluginabi.MethodSchedulerPick, schedReq)
	if err != nil {
		t.Fatalf("scheduler.pick failed: %v", err)
	}
	sResp := schedRes.(pluginapi.SchedulerPickResponse)
	if sResp.Handled {
		t.Fatalf("expected Handled=false when hostDispatched and >=1 candidate is eligible so host SessionAffinitySelector runs, got: %+v", sResp)
	}

	// When host RoundRobin/SessionAffinity dispatches to healthy@example.com, executor keeps it pinned;
	// when dispatched to cooled@example.com on the cooled model, executor unpins so PooledService fails over.
	if !p.ShouldPinExecutorAccount(t.Context(), rt, "healthy@example.com", "AIStudio/gemini-4-argon") {
		t.Fatal("expected healthy@example.com to remain pinned for session affinity")
	}
	if p.ShouldPinExecutorAccount(t.Context(), rt, "cooled@example.com", "AIStudio/gemini-4-argon") {
		t.Fatal("expected cooled@example.com to be unpinned on cooled model so pool can fail over")
	}
}

func TestAssistantModelsOptInGateAndLiveReload(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	t.Setenv("USERPROFILE", tmpDir)
	accDir := filepath.Join(tmpDir, "accounts", "tester@example.com")
	_ = os.MkdirAll(accDir, 0o700)
	_ = os.WriteFile(filepath.Join(accDir, "account.json"), []byte(`{"label":"tester@example.com","enabled":true,"locale":"en-US","timezone":"UTC"}`), 0o600)
	_ = os.WriteFile(filepath.Join(accDir, "storage-state.json"), []byte(`{"cookies":[{"name":"SAPISID","value":"123","domain":".google.com","path":"/","expires":-1},{"name":"__Secure-1PAPISID","value":"456","domain":".google.com","path":"/","expires":-1},{"name":"__Secure-3PAPISID","value":"789","domain":".google.com","path":"/","expires":-1}]}`), 0o600)

	p := cpa.NewPlugin(nil)
	defer func() {
		_, _ = p.HandleMethod(pluginabi.MethodPluginShutdown, nil)
	}()

	baseYAML := "data_dir: \"" + filepath.ToSlash(tmpDir) + "\"\nwaa_backend: spidermonkey\nauto_start: false\nsync_auth_files: false\n"
	regReq, _ := json.Marshal(map[string]any{
		"config_yaml":    []byte(baseYAML),
		"schema_version": pluginabi.SchemaVersion,
	})
	if _, err := p.HandleMethod(pluginabi.MethodPluginRegister, regReq); err != nil {
		t.Fatalf("plugin.register failed: %v", err)
	}

	rt, err := p.Runtime()
	if err != nil {
		t.Fatalf("runtime failed: %v", err)
	}
	if err := rt.Pool().SetCatalog("tester@example.com", aistudio.BenefitTierPro, []aistudio.Model{
		{ID: "gemini-3.8-flash", Name: "Gemini 3.8 Flash", Methods: []string{"generateContent"}, Capabilities: map[string]bool{"chat_model": true}},
		{ID: "gemini-3.8-flash-build", Name: "Gemini 3.8 Flash (Build)", Methods: []string{"generateContent"}, Capabilities: map[string]bool{"chat_model": true, "assistant_route": true}},
	}); err != nil {
		t.Fatalf("SetCatalog failed: %v", err)
	}

	// Default: enable_assistant_models is false, so -build is excluded from catalog and rejected on Generate.
	staticRes, err := p.HandleMethod(pluginabi.MethodModelStatic, nil)
	if err != nil {
		t.Fatalf("model.static failed: %v", err)
	}
	modelResp := staticRes.(pluginapi.ModelResponse)
	for _, m := range modelResp.Models {
		if strings.HasSuffix(m.ID, "-build") {
			t.Fatalf("expected -build model to be hidden when enable_assistant_models=false, got %s", m.ID)
		}
	}

	if !rt.Pool().AssistantModel("gemini-3.8-flash-build") {
		t.Fatal("expected AssistantModel(gemini-3.8-flash-build)=true metadata")
	}
	routeDisabledReq, _ := json.Marshal(pluginapi.ModelRouteRequest{
		RequestedModel: "gemini-3.8-flash-build",
	})
	if routeRes, err := p.HandleMethod(pluginabi.MethodModelRoute, routeDisabledReq); err != nil {
		t.Fatalf("model.route failed: %v", err)
	} else if routeResp := routeRes.(pluginapi.ModelRouteResponse); routeResp.Handled {
		t.Fatalf("expected model.route to reject disabled -build model, got %+v", routeResp)
	}
	if statuses := rt.Pool().Status(); len(statuses) != 1 || len(statuses[0].Models) != 1 {
		t.Fatalf("expected 1 pool status model when assistant disabled, got %+v", statuses)
	}

	// Live reload with enable_assistant_models: true exposes -build with risk warning.
	reloadReq, _ := json.Marshal(map[string]any{
		"config_yaml":    []byte(baseYAML + "enable_assistant_models: true\n"),
		"schema_version": pluginabi.SchemaVersion,
	})
	if _, err := p.HandleMethod(pluginabi.MethodPluginReconfigure, reloadReq); err != nil {
		t.Fatalf("plugin.reconfigure failed: %v", err)
	}
	staticEnabledRes, err := p.HandleMethod(pluginabi.MethodModelStatic, nil)
	if err != nil {
		t.Fatalf("model.static after enable failed: %v", err)
	}
	foundBuild := false
	for _, m := range staticEnabledRes.(pluginapi.ModelResponse).Models {
		if m.ID == "gemini-3.8-flash-build" {
			foundBuild = true
			if !strings.Contains(m.Description, "高风控") {
				t.Fatalf("expected high-risk warning in -build model description, got %q", m.Description)
			}
		}
	}
	if !foundBuild {
		t.Fatal("expected gemini-3.8-flash-build to appear after enable_assistant_models=true")
	}
	if routeRes, err := p.HandleMethod(pluginabi.MethodModelRoute, routeDisabledReq); err != nil {
		t.Fatalf("model.route after enable failed: %v", err)
	} else if routeResp := routeRes.(pluginapi.ModelRouteResponse); !routeResp.Handled {
		t.Fatalf("expected model.route to handle -build model after enable_assistant_models=true, got %+v", routeResp)
	}
	if statuses := rt.Pool().Status(); len(statuses) != 1 || len(statuses[0].Models) != 2 {
		t.Fatalf("expected 2 pool status models after enable_assistant_models=true, got %+v", statuses)
	}
}

func TestMaxRegenerationsConfigAndLiveReload(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	t.Setenv("USERPROFILE", tmpDir)

	// 1. ParsePluginConfig supports top-level max_regenerations=0 and nested options, ignoring negative values.
	_, cfgZero, err := cpa.ParsePluginConfig([]byte("data_dir: \"" + filepath.ToSlash(tmpDir) + "\"\nmax_regenerations: 0\n"))
	if err != nil {
		t.Fatalf("ParsePluginConfig(0) failed: %v", err)
	}
	if cfgZero.MaxRegenerations != 0 {
		t.Fatalf("expected MaxRegenerations=0, got %d", cfgZero.MaxRegenerations)
	}

	_, cfgNested, err := cpa.ParsePluginConfig([]byte("data_dir: \"" + filepath.ToSlash(tmpDir) + "\"\noptions:\n  max_regenerations: 5\n"))
	if err != nil {
		t.Fatalf("ParsePluginConfig(nested) failed: %v", err)
	}
	if cfgNested.MaxRegenerations != 5 {
		t.Fatalf("expected nested options MaxRegenerations=5, got %d", cfgNested.MaxRegenerations)
	}

	_, cfgNeg, err := cpa.ParsePluginConfig([]byte("data_dir: \"" + filepath.ToSlash(tmpDir) + "\"\nmax_regenerations: -1\n"))
	if err != nil {
		t.Fatalf("ParsePluginConfig(-1) failed: %v", err)
	}
	if cfgNeg.MaxRegenerations != 3 {
		t.Fatalf("expected negative max_regenerations to keep default 3, got %d", cfgNeg.MaxRegenerations)
	}

	// 2. Plugin registration & reconfigure update live runtime MaxRegenerations.
	p := cpa.NewPlugin(nil)
	defer func() {
		_, _ = p.HandleMethod(pluginabi.MethodPluginShutdown, nil)
	}()
	baseYAML := "data_dir: \"" + filepath.ToSlash(tmpDir) + "\"\nwaa_backend: spidermonkey\nauto_start: false\nsync_auth_files: false\n"
	regReq, _ := json.Marshal(map[string]any{
		"config_yaml":    []byte(baseYAML + "max_regenerations: 0\n"),
		"schema_version": pluginabi.SchemaVersion,
	})
	if _, err := p.HandleMethod(pluginabi.MethodPluginRegister, regReq); err != nil {
		t.Fatalf("plugin.register failed: %v", err)
	}
	rt, err := p.Runtime()
	if err != nil {
		t.Fatalf("Runtime() failed: %v", err)
	}
	if got := rt.MaxRegenerations(); got != 0 {
		t.Fatalf("expected initial live MaxRegenerations=0, got %d", got)
	}

	reconfReq, _ := json.Marshal(map[string]any{
		"config_yaml":    []byte(baseYAML + "max_regenerations: 6\n"),
		"schema_version": pluginabi.SchemaVersion,
	})
	if _, err := p.HandleMethod(pluginabi.MethodPluginReconfigure, reconfReq); err != nil {
		t.Fatalf("plugin.reconfigure failed: %v", err)
	}
	if got := rt.MaxRegenerations(); got != 6 {
		t.Fatalf("expected live MaxRegenerations=6 after reconfigure, got %d", got)
	}
}

func TestWebUISessionStorageLogsLimitAndHostLogBridge(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	t.Setenv("USERPROFILE", tmpDir)

	var logEventsMu sync.Mutex
	var hostLogPayloads []string
	logSignal := make(chan struct{}, 4)

	p := cpa.NewPlugin(func(method string, payload []byte) ([]byte, error) {
		if method == pluginabi.MethodHostLog {
			logEventsMu.Lock()
			hostLogPayloads = append(hostLogPayloads, string(payload))
			logEventsMu.Unlock()
			select {
			case logSignal <- struct{}{}:
			default:
			}
		}
		return json.Marshal(pluginabi.Envelope{OK: true, Result: json.RawMessage(`{}`)})
	})
	defer func() {
		_, _ = p.HandleMethod(pluginabi.MethodPluginShutdown, nil)
	}()

	regReq, _ := json.Marshal(map[string]any{
		"config_yaml":    []byte("data_dir: \"" + filepath.ToSlash(tmpDir) + "\"\nwaa_backend: spidermonkey\nauto_start: false\nsync_auth_files: false\n"),
		"schema_version": pluginabi.SchemaVersion,
	})
	if _, err := p.HandleMethod(pluginabi.MethodPluginRegister, regReq); err != nil {
		t.Fatalf("plugin.register failed: %v", err)
	}

	// 1. Verify WebUI strictly uses sessionStorage (never localStorage.setItem), visibility throttling, and ?limit=80
	uiReq, _ := json.Marshal(pluginapi.ManagementRequest{
		Method: http.MethodGet,
		Path:   "/v0/resource/plugins/aistudio2api/accounts",
	})
	uiRes, err := p.HandleMethod(pluginabi.MethodManagementHandle, uiReq)
	if err != nil {
		t.Fatalf("webui handle failed: %v", err)
	}
	uiHTML := string(uiRes.(pluginapi.ManagementResponse).Body)
	if strings.Contains(uiHTML, "localStorage.setItem") {
		t.Fatal("webui.html must not persist management keys in localStorage")
	}
	for _, needle := range []string{"sessionStorage.setItem", "document.hidden", "visibilitychange", "/v0/management/aistudio2api/logs?limit=80"} {
		if !strings.Contains(uiHTML, needle) {
			t.Fatalf("expected %q in webui.html", needle)
		}
	}

	// 2. Record both an INFO access log and an ERROR access log via rt.Admin.RecordAccessLog,
	// verifying structured host.log bridging (with model/status/error fields) and ?limit=1 pagination.
	rt, err := p.Runtime()
	if err != nil {
		t.Fatalf("Runtime() failed: %v", err)
	}
	rt.Admin.RecordAccessLog(api.AccessLog{
		RequestID: "req-ok-1",
		Model:     "gemini-3.8-flash",
		Method:    http.MethodPost,
		Path:      "/v1/chat/completions",
		Status:    http.StatusOK,
	})
	rt.Admin.RecordAccessLog(api.AccessLog{
		RequestID: "req-err-2",
		Model:     "gemini-4-argon",
		Method:    http.MethodPost,
		Path:      "/v1/chat/completions",
		Status:    http.StatusBadGateway,
		Error:     "upstream stream interrupted",
	})
	select {
	case <-logSignal:
	case <-time.After(2 * time.Second):
		t.Fatal("expected runtime WARN/ERROR log to bridge to host.log callback")
	}
	logEventsMu.Lock()
	gotHostLogs := append([]string(nil), hostLogPayloads...)
	logEventsMu.Unlock()
	if len(gotHostLogs) == 0 || (!strings.Contains(gotHostLogs[0], `"level":"warn"`) && !strings.Contains(gotHostLogs[0], `"level":"error"`)) {
		t.Fatalf("unexpected host.log payload: %v", gotHostLogs)
	}

	allLogs := rt.RecentLogs()
	if len(allLogs) < 2 {
		t.Fatalf("expected >= 2 runtime logs, got %d", len(allLogs))
	}
	logsReq, _ := json.Marshal(pluginapi.ManagementRequest{
		Method: http.MethodGet,
		Path:   "/v0/management/aistudio2api/logs?limit=1",
	})
	logsRes, err := p.HandleMethod(pluginabi.MethodManagementHandle, logsReq)
	if err != nil {
		t.Fatalf("GET /logs?limit=1 failed: %v", err)
	}
	var logsBody struct {
		Logs []any `json:"logs"`
	}
	if err := json.Unmarshal(logsRes.(pluginapi.ManagementResponse).Body, &logsBody); err != nil {
		t.Fatalf("decode /logs?limit=1 response: %v", err)
	}
	if len(logsBody.Logs) != 1 {
		t.Fatalf("expected exactly 1 log entry from ?limit=1, got %d", len(logsBody.Logs))
	}
}

func TestAuthRequiredParseSkipsStaleCookiesAndRecoversOnNewCookies(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	t.Setenv("USERPROFILE", tmpDir)

	p := cpa.NewPlugin(nil)
	defer func() {
		_, _ = p.HandleMethod(pluginabi.MethodPluginShutdown, nil)
	}()

	regReq, _ := json.Marshal(map[string]any{
		"config_yaml":    []byte("data_dir: \"" + filepath.ToSlash(tmpDir) + "\"\nwaa_backend: spidermonkey\nauto_start: true\nsync_auth_files: false\n"),
		"schema_version": pluginabi.SchemaVersion,
	})
	if _, err := p.HandleMethod(pluginabi.MethodPluginRegister, regReq); err != nil {
		t.Fatalf("plugin.register failed: %v", err)
	}

	rt, err := p.Runtime()
	if err != nil {
		t.Fatalf("Runtime() failed: %v", err)
	}

	makeAuthParseReq := func(sapisid string) []byte {
		payload := map[string]any{
			"type":       "aistudio2api",
			"provider":   "aistudio2api",
			"account_id": "user@example.com",
			"email":      "user@example.com",
			"proxy":      "http://127.0.0.1:1",
			"locale":     "en-US",
			"timezone":   "UTC",
			"storage_state": map[string]any{
				"cookies": []map[string]any{
					{"name": "SAPISID", "value": sapisid, "domain": ".google.com", "path": "/", "expires": -1},
					{"name": "__Secure-1PAPISID", "value": sapisid, "domain": ".google.com", "path": "/", "expires": -1},
					{"name": "__Secure-3PAPISID", "value": sapisid, "domain": ".google.com", "path": "/", "expires": -1},
					{"name": "__Secure-1PSID", "value": "psid-1", "domain": ".google.com", "path": "/", "expires": -1},
					{"name": "__Secure-3PSID", "value": "psid-3", "domain": ".google.com", "path": "/", "expires": -1},
					{"name": "SID", "value": "sid-1", "domain": ".google.com", "path": "/", "expires": -1},
				},
			},
		}
		rawJSON, _ := json.Marshal(payload)
		req, _ := json.Marshal(pluginapi.AuthParseRequest{
			FileName: "aistudio2api-user@example.com.json",
			RawJSON:  rawJSON,
		})
		return req
	}

	res, err := p.HandleMethod(pluginabi.MethodAuthParse, makeAuthParseReq("old-sapisid"))
	if err != nil {
		t.Fatalf("initial auth.parse failed: %v", err)
	}
	parsed := res.(pluginapi.AuthParseResponse)
	if !parsed.Handled || parsed.Auth.Disabled {
		t.Fatalf("expected handled enabled auth on initial parse, got %+v", parsed)
	}

	poolAcc, ok := rt.Pool().AccountByID("user@example.com")
	if !ok || poolAcc == nil {
		t.Fatal("expected account in pool")
	}
	poolAcc.State = aistudio.AccountAuthRequired

	// Re-parsing the same dead cookies must preserve AccountAuthRequired and keep host auth disabled.
	res, err = p.HandleMethod(pluginabi.MethodAuthParse, makeAuthParseReq("old-sapisid"))
	if err != nil {
		t.Fatalf("stale auth.parse failed: %v", err)
	}
	if poolAcc.State != aistudio.AccountAuthRequired {
		t.Fatalf("expected AccountAuthRequired preserved for identical cookies, got %s", poolAcc.State)
	}
	if staleParsed := res.(pluginapi.AuthParseResponse); !staleParsed.Auth.Disabled {
		t.Fatalf("expected host auth disabled while AccountAuthRequired, got %+v", staleParsed.Auth)
	}

	// Host auth.refresh while AccountAuthRequired must also preserve state and stay disabled.
	refreshReq, _ := json.Marshal(pluginapi.AuthRefreshRequest{
		AuthID:     "aistudio2api-user@example.com.json",
		Attributes: map[string]string{"account_id": "user@example.com"},
	})
	refreshRes, err := p.HandleMethod(pluginabi.MethodAuthRefresh, refreshReq)
	if err != nil {
		t.Fatalf("auth.refresh failed: %v", err)
	}
	if refreshed := refreshRes.(pluginapi.AuthRefreshResponse); !refreshed.Auth.Disabled || poolAcc.State != aistudio.AccountAuthRequired {
		t.Fatalf("expected disabled auth and AccountAuthRequired during auth.refresh, got %+v state=%s", refreshed.Auth, poolAcc.State)
	}

	// Updating cookies via auth.parse must re-import and clear AccountAuthRequired.
	res, err = p.HandleMethod(pluginabi.MethodAuthParse, makeAuthParseReq("fresh-sapisid"))
	if err != nil {
		t.Fatalf("fresh auth.parse failed: %v", err)
	}
	if poolAcc.State == aistudio.AccountAuthRequired {
		t.Fatalf("expected fresh cookies to clear AccountAuthRequired, got %s", poolAcc.State)
	}
}

func TestManagementAccountStateRestoreAndAuthSync(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	t.Setenv("USERPROFILE", tmpDir)
	authDir := filepath.Join(tmpDir, ".cli-proxy-api")
	if err := os.MkdirAll(authDir, 0o700); err != nil {
		t.Fatalf("mkdir authDir: %v", err)
	}

	p := cpa.NewPlugin(nil)
	defer func() {
		_, _ = p.HandleMethod(pluginabi.MethodPluginShutdown, nil)
	}()

	regReq, _ := json.Marshal(map[string]any{
		"config_yaml":    []byte("data_dir: \"" + filepath.ToSlash(tmpDir) + "\"\nwaa_backend: spidermonkey\nauto_start: false\nsync_auth_files: true\n"),
		"schema_version": pluginabi.SchemaVersion,
	})
	if _, err := p.HandleMethod(pluginabi.MethodPluginRegister, regReq); err != nil {
		t.Fatalf("plugin.register failed: %v", err)
	}
	rt, err := p.Runtime()
	if err != nil {
		t.Fatalf("Runtime() failed: %v", err)
	}

	payload := map[string]any{
		"type":       "aistudio2api",
		"provider":   "aistudio2api",
		"account_id": "restore@example.com",
		"email":      "restore@example.com",
		"proxy":      "http://127.0.0.1:1",
		"locale":     "en-US",
		"timezone":   "UTC",
		"storage_state": map[string]any{
			"cookies": []map[string]any{
				{"name": "SAPISID", "value": "sap-1", "domain": ".google.com", "path": "/", "expires": -1},
				{"name": "__Secure-1PAPISID", "value": "sap-1", "domain": ".google.com", "path": "/", "expires": -1},
				{"name": "__Secure-3PAPISID", "value": "sap-1", "domain": ".google.com", "path": "/", "expires": -1},
				{"name": "__Secure-1PSID", "value": "psid-1", "domain": ".google.com", "path": "/", "expires": -1},
				{"name": "__Secure-3PSID", "value": "psid-3", "domain": ".google.com", "path": "/", "expires": -1},
				{"name": "SID", "value": "sid-1", "domain": ".google.com", "path": "/", "expires": -1},
			},
		},
	}
	rawJSON, _ := json.Marshal(payload)
	parseReq, _ := json.Marshal(pluginapi.AuthParseRequest{
		FileName: "aistudio2api-restore@example.com.json",
		RawJSON:  rawJSON,
	})
	if _, err := p.HandleMethod(pluginabi.MethodAuthParse, parseReq); err != nil {
		t.Fatalf("auth.parse failed: %v", err)
	}

	if err := rt.Pool().MarkRestricted("restore@example.com", &camoufoxnative.AccessDenial{Err: camoufoxnative.ErrServiceRestricted}); err != nil {
		t.Fatalf("MarkRestricted failed: %v", err)
	}
	if p.ShouldPinExecutorAccount(t.Context(), rt, "restore@example.com", "gemini-3.8-flash") {
		t.Fatal("expected restricted account not to be pinned")
	}
	authFilePath := filepath.Join(authDir, "aistudio2api-restore@example.com.json")
	checkHostDisabled := func(want bool) {
		t.Helper()
		raw, err := os.ReadFile(authFilePath)
		if err != nil {
			t.Fatalf("read host auth file: %v", err)
		}
		var doc struct {
			Disabled bool `json:"disabled"`
		}
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Fatalf("decode host auth file: %v", err)
		}
		if doc.Disabled != want {
			t.Fatalf("host auth disabled = %v, want %v (json=%s)", doc.Disabled, want, string(raw))
		}
	}
	checkHostDisabled(true)

	callState := func(state string) (pluginapi.ManagementResponse, api.AdminAccount) {
		body, _ := json.Marshal(map[string]string{
			"account_id": "restore@example.com",
			"state":      state,
		})
		req, _ := json.Marshal(pluginapi.ManagementRequest{
			Method: http.MethodPost,
			Path:   "/v0/management/aistudio2api/accounts/state",
			Body:   body,
		})
		res, err := p.HandleMethod(pluginabi.MethodManagementHandle, req)
		if err != nil {
			t.Fatalf("accounts/state call failed: %v", err)
		}
		mres := res.(pluginapi.ManagementResponse)
		var out struct {
			Account api.AdminAccount `json:"account"`
		}
		_ = json.Unmarshal(mres.Body, &out)
		return mres, out.Account
	}

	poolAcc, ok := rt.Pool().AccountByID("restore@example.com")
	if !ok || poolAcc == nil {
		t.Fatal("expected restore@example.com in pool")
	}
	isReadyOrSyncing := func(state string) bool {
		return state == string(aistudio.AccountReady) || state == "busy"
	}

	// Restore restricted -> ready: clears restriction, syncs host auth disabled=false, restores pinning.
	res, acc := callState("ready")
	if res.StatusCode != http.StatusOK || !acc.Enabled || !isReadyOrSyncing(acc.State) || poolAcc.State != aistudio.AccountReady {
		t.Fatalf("expected restored ready account, status=%d acc=%+v poolState=%s body=%s", res.StatusCode, acc, poolAcc.State, string(res.Body))
	}
	checkHostDisabled(false)
	if !p.ShouldPinExecutorAccount(t.Context(), rt, "restore@example.com", "gemini-3.8-flash") {
		t.Fatal("expected restored account to be pinned again")
	}

	// Disable via accounts/state -> disabled.
	res, acc = callState("disabled")
	if res.StatusCode != http.StatusOK || acc.Enabled || acc.State != "disabled" {
		t.Fatalf("expected disabled account, status=%d acc=%+v body=%s", res.StatusCode, acc, string(res.Body))
	}
	checkHostDisabled(true)

	// Re-enable & restore via accounts/state -> ready.
	res, acc = callState("ready")
	if res.StatusCode != http.StatusOK || !acc.Enabled || !isReadyOrSyncing(acc.State) || poolAcc.State != aistudio.AccountReady {
		t.Fatalf("expected re-enabled ready account, status=%d acc=%+v poolState=%s body=%s", res.StatusCode, acc, poolAcc.State, string(res.Body))
	}
	checkHostDisabled(false)
	// Invalid state must return 400.
	res, _ = callState("bogus")
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for invalid state, got %d body=%s", res.StatusCode, string(res.Body))
	}

	// Verify WebUI renders the restore-account action and status/account transition hints.
	uiReq, _ := json.Marshal(pluginapi.ManagementRequest{
		Method: http.MethodGet,
		Path:   "/v0/resource/plugins/aistudio2api/accounts",
	})
	uiRes, err := p.HandleMethod(pluginabi.MethodManagementHandle, uiReq)
	if err != nil {
		t.Fatalf("webui handle failed: %v", err)
	}
	uiHTML := string(uiRes.(pluginapi.ManagementResponse).Body)
	for _, needle := range []string{"restore-account", "/v0/management/aistudio2api/accounts/state", "transitionLabels", "catalog_error"} {
		if !strings.Contains(uiHTML, needle) {
			t.Fatalf("expected %q in webui.html", needle)
		}
	}
}

func TestExecutorResponsesMCPSSRFBlockedEndToEnd(t *testing.T) {
	var hits atomic.Int32
	victim := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"secret":"host-management-api"}`))
	}))
	defer victim.Close()

	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	t.Setenv("USERPROFILE", tmpDir)

	p := cpa.NewPlugin(nil)
	defer func() {
		_, _ = p.HandleMethod(pluginabi.MethodPluginShutdown, nil)
	}()

	regReq, _ := json.Marshal(map[string]any{
		"config_yaml":    []byte("data_dir: \"" + filepath.ToSlash(tmpDir) + "\"\nwaa_backend: spidermonkey\nauto_start: false\nsync_auth_files: false\n"),
		"schema_version": pluginabi.SchemaVersion,
	})
	regRes, err := p.HandleMethod(pluginabi.MethodPluginRegister, regReq)
	if err != nil {
		t.Fatalf("plugin.register failed: %v", err)
	}

	// Verify allowed_mcp_hosts and allowed_webhook_hosts are exposed in ConfigFields and default to empty (deny-all).
	rawReg, _ := json.Marshal(regRes)
	var parsedReg struct {
		Metadata pluginapi.Metadata `json:"metadata"`
	}
	if err := json.Unmarshal(rawReg, &parsedReg); err != nil {
		t.Fatalf("decode registration: %v", err)
	}
	foundFields := map[string]any{}
	for _, f := range parsedReg.Metadata.ConfigFields {
		foundFields[f.Name] = f.Default
	}
	for _, key := range []string{"allowed_mcp_hosts", "allowed_webhook_hosts"} {
		def, ok := foundFields[key]
		if !ok {
			t.Fatalf("expected ConfigField %q in registration metadata", key)
		}
		if def != "" {
			t.Fatalf("expected ConfigField %q default to be empty string, got %#v", key, def)
		}
	}

	// End-to-end executor.execute -> /v1/responses -> MCP SSRF probe must be blocked with 0 hits.
	payload := []byte(`{"model":"gemini-3.8-flash","input":"probe","tools":[{"type":"mcp","server_label":"cpa","server_url":"` + victim.URL + `","headers":{"Authorization":"Bearer mgmt-key"}}]}`)
	execReq, _ := json.Marshal(pluginapi.ExecutorRequest{
		Model:   "gemini-3.8-flash",
		Format:  "responses",
		Payload: payload,
	})
	if _, err := p.HandleMethod(pluginabi.MethodExecutorExecute, execReq); err == nil {
		t.Fatal("expected executor.execute Responses MCP SSRF probe to fail, got nil error")
	}
	if hits.Load() != 0 {
		t.Fatalf("expected 0 outbound hits to loopback victim via executor.execute, got %d", hits.Load())
	}

	// Verify executor.http_request blocks /api path traversal (/v1/../api/status) with 403.
	httpReq, _ := json.Marshal(pluginapi.ExecutorHTTPRequest{
		Method: http.MethodGet,
		URL:    "/v1/../api/status",
	})
	httpRes, err := p.HandleMethod(pluginabi.MethodExecutorHTTPRequest, httpReq)
	if err != nil {
		t.Fatalf("executor.http_request failed: %v", err)
	}
	if got := httpRes.(pluginapi.ExecutorHTTPResponse).StatusCode; got != http.StatusForbidden {
		t.Fatalf("expected 403 for /v1/../api/status path traversal, got %d", got)
	}

	// Verify live reload updates allowed_mcp_hosts without still allowing loopback.
	reloadReq, _ := json.Marshal(map[string]any{
		"config_yaml": []byte("data_dir: \"" + filepath.ToSlash(tmpDir) + "\"\nwaa_backend: spidermonkey\nauto_start: false\nsync_auth_files: false\nallowed_mcp_hosts: \"mcp.trusted.example\"\n"),
	})
	if _, err := p.HandleMethod(pluginabi.MethodPluginReconfigure, reloadReq); err != nil {
		t.Fatalf("plugin.reconfigure failed: %v", err)
	}
	if got := p.Config().AllowedMCPHosts; got != "mcp.trusted.example" {
		t.Fatalf("expected AllowedMCPHosts=mcp.trusted.example after reload, got %q", got)
	}
	if _, err := p.HandleMethod(pluginabi.MethodExecutorExecute, execReq); err == nil {
		t.Fatal("expected loopback MCP SSRF probe to remain blocked after reload")
	}
	if hits.Load() != 0 {
		t.Fatalf("expected 0 outbound hits after reload, got %d", hits.Load())
	}
}

func TestPluginConfigSillyTavernAndFirstEventTimeout(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	t.Setenv("USERPROFILE", tmpDir)
	raw := []byte("data_dir: \"" + filepath.ToSlash(tmpDir) + "\"\nwaa_backend: spidermonkey\nauto_start: false\nsync_auth_files: false\nfirst_event_timeout: \"45\"\nreply_tool: true\nuser_message_rewrite: martian\nthinking_guard: true\n")
	_, cfg, err := cpa.ParsePluginConfig(raw)
	if err != nil {
		t.Fatalf("ParsePluginConfig failed: %v", err)
	}
	if cfg.FirstEventTimeout != "45" || !cfg.ReplyTool || cfg.UserMessageRewrite != "martian" || !cfg.ThinkingGuard {
		t.Fatalf("unexpected parsed config: timeout=%q reply=%v rewrite=%q guard=%v", cfg.FirstEventTimeout, cfg.ReplyTool, cfg.UserMessageRewrite, cfg.ThinkingGuard)
	}

	p := cpa.NewPlugin(nil)
	t.Cleanup(func() { _, _ = p.HandleMethod(pluginabi.MethodPluginShutdown, nil) })
	regReq, _ := json.Marshal(map[string]any{"config_yaml": raw, "schema_version": pluginabi.SchemaVersion})
	regRes, err := p.HandleMethod(pluginabi.MethodPluginRegister, regReq)
	if err != nil {
		t.Fatalf("plugin.register failed: %v", err)
	}
	rawReg, _ := json.Marshal(regRes)
	var parsedReg struct {
		Metadata pluginapi.Metadata `json:"metadata"`
	}
	if err := json.Unmarshal(rawReg, &parsedReg); err != nil {
		t.Fatalf("decode registration: %v", err)
	}
	names := make(map[string]bool, len(parsedReg.Metadata.ConfigFields))
	for _, f := range parsedReg.Metadata.ConfigFields {
		names[f.Name] = true
	}
	for _, required := range []string{"first_event_timeout", "reply_tool", "user_message_rewrite", "thinking_guard"} {
		if !names[required] {
			t.Fatalf("expected ConfigField %q in plugin registration", required)
		}
	}
}
