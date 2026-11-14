package cpa_test

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/cpa"
	"github.com/Mag1cFall/AIStudio2API/internal/pluginabi"
	"github.com/Mag1cFall/AIStudio2API/internal/pluginapi"
	"github.com/Mag1cFall/AIStudio2API/internal/waa"
)

func TestPluginLifecycleAndCapabilities(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	var hostCalls []string
	p := cpa.NewPlugin(func(method string, payload []byte) ([]byte, error) {
		hostCalls = append(hostCalls, method)
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
	dylibPath := filepath.Join("..", "..", "dist", "plugins", "darwin", "arm64", "aistudio2api.dylib")
	info, err := os.Stat(dylibPath)
	if err != nil {
		t.Skipf("dylib not found at %s: %v", dylibPath, err)
	}
	if info.Size() < 10*1024*1024 {
		t.Fatalf("expected static-linked SpiderMonkey dylib > 10MB, got %d bytes", info.Size())
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
