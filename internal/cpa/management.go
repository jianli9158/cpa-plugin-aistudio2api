package cpa

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/api"
	"github.com/Mag1cFall/AIStudio2API/internal/app"
	"github.com/Mag1cFall/AIStudio2API/internal/pluginapi"
)

type rpcManagementRequest struct {
	pluginapi.ManagementRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

type rpcManagementRegistrationResponse struct {
	Routes    []pluginapi.ManagementRoute `json:"routes,omitempty"`
	Resources []pluginapi.ResourceRoute   `json:"resources,omitempty"`
}

func (p *Plugin) handleCLIRegister() (any, error) {
	return pluginapi.CommandLineRegistrationResponse{
		Flags: []pluginapi.CommandLineFlag{
			{
				Name:         "aistudio2api-login",
				Type:         "bool",
				DefaultValue: "false",
				Usage:        "Launch local Chrome Incognito window to sign in to Google AI Studio and import account",
			},
			{
				Name:         "aistudio2api-status",
				Type:         "bool",
				DefaultValue: "false",
				Usage:        "Print AIStudio2API plugin account pool and SpiderMonkey runtime status",
			},
		},
	}, nil
}

func (p *Plugin) handleCLIRun(raw []byte) (any, error) {
	var req pluginapi.CommandLineExecutionRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	rt, err := p.Runtime()
	if err != nil {
		return pluginapi.CommandLineExecutionResponse{
			ExitCode: 1,
			Stderr:   []byte("Failed to initialize AIStudio2API runtime: " + err.Error() + "\n"),
		}, nil
	}

	if flag, ok := req.TriggeredFlags["aistudio2api-login"]; ok && flag.Set && strings.EqualFold(flag.Value, "true") {
		created, err := rt.Admin.CreateAccount(p.ctx, api.AccountCreateInput{
			Proxy: p.appCfg.Proxy,
		}, nil)
		if err != nil {
			return pluginapi.CommandLineExecutionResponse{
				ExitCode: 1,
				Stderr:   []byte("Chrome Incognito login failed: " + err.Error() + "\n"),
			}, nil
		}
		_ = p.SyncAccountToHostAuth("", created)
		out, _ := json.MarshalIndent(redactAdminAccount(created), "", "  ")
		return pluginapi.CommandLineExecutionResponse{
			ExitCode: 0,
			Stdout:   []byte("Successfully logged in and imported AI Studio account:\n" + string(out) + "\n"),
		}, nil
	}

	if flag, ok := req.TriggeredFlags["aistudio2api-status"]; ok && flag.Set && strings.EqualFold(flag.Value, "true") {
		status, _ := rt.Admin.Status(p.ctx)
		accounts, _ := rt.Admin.Accounts(p.ctx)
		out, _ := json.MarshalIndent(map[string]any{
			"status":   status,
			"accounts": redactAdminAccounts(accounts),
		}, "", "  ")
		return pluginapi.CommandLineExecutionResponse{
			ExitCode: 0,
			Stdout:   []byte(string(out) + "\n"),
		}, nil
	}

	return pluginapi.CommandLineExecutionResponse{ExitCode: 0}, nil
}

func (p *Plugin) handleManagementRegister() (any, error) {
	return rpcManagementRegistrationResponse{
		Routes: []pluginapi.ManagementRoute{
			{Method: http.MethodGet, Path: "/v0/management/aistudio2api/status", Description: "Get AIStudio2API runtime status, accounts, models, and cooldowns"},
			{Method: http.MethodPost, Path: "/v0/management/aistudio2api/service/start", Description: "Start AIStudio2API generation service and SpiderMonkey WAA workers"},
			{Method: http.MethodPost, Path: "/v0/management/aistudio2api/service/stop", Description: "Stop AIStudio2API generation service"},
			{Method: http.MethodGet, Path: "/v0/management/aistudio2api/accounts", Description: "List AIStudio2API accounts"},
			{Method: http.MethodPost, Path: "/v0/management/aistudio2api/accounts", Description: "Import AIStudio2API account from Cookie header or StorageState JSON"},
			{Method: http.MethodPost, Path: "/v0/management/aistudio2api/accounts/login-browser", Description: "Launch local Chrome Incognito window to sign in to Google AI Studio and auto-import account"},
			{Method: http.MethodPost, Path: "/v0/management/aistudio2api/accounts/relogin", Description: "Re-authenticate an existing AIStudio2API account via local Chrome Incognito window"},
			{Method: http.MethodPost, Path: "/v0/management/aistudio2api/accounts/discover-cookies", Description: "Discover Google accounts inside pasted Cookie text"},
			{Method: http.MethodPost, Path: "/v0/management/aistudio2api/accounts/refresh", Description: "Refresh an account's model catalog and benefit tier"},
			{Method: http.MethodPost, Path: "/v0/management/aistudio2api/accounts/update", Description: "Update account settings (enabled, proxy, locale, timezone)"},
			{Method: http.MethodPost, Path: "/v0/management/aistudio2api/accounts/delete", Description: "Delete an account"},
			{Method: http.MethodGet, Path: "/v0/management/aistudio2api/accounts/exits", Description: "Inspect egress IP, country, timezone, and locale per configured proxy"},
			{Method: http.MethodPost, Path: "/v0/management/aistudio2api/accounts/sync-auth", Description: "Sync all AIStudio2API accounts to CLIProxyAPI auths/ directory"},
			{Method: http.MethodPost, Path: "/v0/management/aistudio2api/cooldowns/clear", Description: "Clear account cooldowns"},
			{Method: http.MethodGet, Path: "/v0/management/aistudio2api/models", Description: "List discovered AI Studio models"},
			{Method: http.MethodGet, Path: "/v0/management/aistudio2api/logs", Description: "List recent AIStudio2API logs"},
		},
		Resources: []pluginapi.ResourceRoute{
			{
				Path:        "/accounts",
				Menu:        "AI Studio (WAA)",
				Description: "AI Studio 账户、SpiderMonkey WAA 与冷却管理",
			},
		},
	}, nil
}

func (p *Plugin) handleManagementHandle(raw []byte) (any, error) {
	var req rpcManagementRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}

	rawPath := strings.TrimSpace(req.Path)
	path := rawPath
	if idx := strings.IndexByte(rawPath, '?'); idx >= 0 {
		path = rawPath[:idx]
	}
	method := strings.ToUpper(strings.TrimSpace(req.Method))
	if method == "" {
		method = http.MethodGet
	}

	// Serve static Web Resource UI only (strictly passive HTML/CSS/JS per CPA Route Boundary policy)
	if path == "/accounts" || path == "/" || strings.HasPrefix(path, "/v0/resource/plugins/aistudio2api") {
		return pluginapi.ManagementResponse{
			StatusCode: http.StatusOK,
			Headers: http.Header{
				"Content-Type":  []string{"text/html; charset=utf-8"},
				"Cache-Control": []string{"no-store"},
			},
			Body: []byte(webUIDashboardHTML),
		}, nil
	}
	if strings.HasPrefix(path, "/aistudio2api/") {
		path = "/v0/management" + path
	}
	rt, err := p.Runtime()
	if err != nil {
		return jsonManagementResponse(http.StatusInternalServerError, map[string]any{
			"error": err.Error(),
		}), nil
	}

	switch {
	case method == http.MethodGet && path == "/v0/management/aistudio2api/status":
		return p.buildStatusManagementResponse(rt), nil
	case method == http.MethodPost && path == "/v0/management/aistudio2api/service/start":
		status, err := rt.Admin.StartService(p.ctx)
		if err != nil {
			return jsonManagementResponse(http.StatusBadGateway, map[string]any{"error": err.Error(), "status": status}), nil
		}
		p.serviceStopped.Store(false)
		if boolOrDefault(p.Config().SyncAuthFiles, true) {
			p.setHostAuthFilesDisabled(rt, false)
		}
		return jsonManagementResponse(http.StatusOK, map[string]any{"status": status}), nil

	case method == http.MethodPost && path == "/v0/management/aistudio2api/service/stop":
		p.serviceStopped.Store(true)
		status, err := rt.Admin.StopService(p.ctx)
		if err != nil {
			return jsonManagementResponse(http.StatusBadGateway, map[string]any{"error": err.Error(), "status": status}), nil
		}
		if boolOrDefault(p.Config().SyncAuthFiles, true) {
			p.setHostAuthFilesDisabled(rt, true)
		}
		return jsonManagementResponse(http.StatusOK, map[string]any{"status": status}), nil
	case method == http.MethodGet && path == "/v0/management/aistudio2api/accounts":
		accounts, err := rt.Admin.Accounts(p.ctx)
		if err != nil {
			return jsonManagementResponse(http.StatusInternalServerError, map[string]any{"error": err.Error()}), nil
		}
		return jsonManagementResponse(http.StatusOK, map[string]any{"accounts": redactAdminAccounts(accounts)}), nil
	case method == http.MethodGet && path == "/v0/management/aistudio2api/accounts/exits":
		exits, err := rt.Admin.AccountExits(p.ctx)
		if err != nil {
			return jsonManagementResponse(http.StatusBadGateway, map[string]any{"error": err.Error()}), nil
		}
		for i := range exits {
			exits[i].Proxy = redactProxyURLString(exits[i].Proxy)
		}
		return jsonManagementResponse(http.StatusOK, map[string]any{"exits": exits}), nil

	case method == http.MethodPost && path == "/v0/management/aistudio2api/accounts/login-browser":
		if !p.chromeLogin.TryLock() {
			return jsonManagementResponse(http.StatusConflict, map[string]any{"error": "已有一个本机 Chrome 无痕登录窗口正在进行中，请在该窗口完成登录或关闭它后重试"}), nil
		}
		defer p.chromeLogin.Unlock()
		var input api.AccountCreateInput
		if len(req.Body) > 0 {
			_ = json.Unmarshal(req.Body, &input)
		}
		created, err := rt.Admin.CreateAccount(p.ctx, input, nil)
		if err != nil {
			return jsonManagementResponse(http.StatusBadRequest, map[string]any{"error": err.Error()}), nil
		}
		if created.Enabled {
			p.serviceStopped.Store(false)
		}
		_ = p.SyncAccountToHostAuth(req.HostCallbackID, created)
		go func() { _ = rt.EnsureRunning(p.ctx) }()
		return jsonManagementResponse(http.StatusCreated, map[string]any{"account": redactAdminAccount(created)}), nil
	case method == http.MethodPost && path == "/v0/management/aistudio2api/accounts/relogin":
		if !p.chromeLogin.TryLock() {
			return jsonManagementResponse(http.StatusConflict, map[string]any{"error": "已有一个本机 Chrome 无痕登录窗口正在进行中，请在该窗口完成登录或关闭它后重试"}), nil
		}
		defer p.chromeLogin.Unlock()
		var body struct {
			AccountID string  `json:"account_id"`
			Proxy     *string `json:"proxy,omitempty"`
		}
		if len(req.Body) > 0 {
			_ = json.Unmarshal(req.Body, &body)
		}
		accountID := strings.TrimSpace(body.AccountID)
		var updated api.AdminAccount
		var err error
		if accountID != "" {
			if body.Proxy != nil {
				if existing, eErr := rt.Admin.Accounts(p.ctx); eErr == nil {
					for _, acc := range existing {
						if strings.EqualFold(acc.ID, accountID) {
							if resolvedProxy, changed := resolveUpdatedProxy(body.Proxy, acc.Proxy); changed {
								locale := acc.Locale
								if locale == "" {
									locale = "en-US"
								}
								tz := acc.Timezone
								if tz == "" {
									tz = "America/Los_Angeles"
								}
								_, _ = rt.Admin.UpdateAccount(p.ctx, accountID, api.AccountInput{
									Label:    accountID,
									Enabled:  acc.Enabled,
									Proxy:    resolvedProxy,
									Locale:   locale,
									Timezone: tz,
								})
							}
							break
						}
					}
				}
			}
			updated, err = rt.Admin.LoginAccount(p.ctx, accountID, nil)
		} else {
			proxyVal := ""
			if body.Proxy != nil {
				proxyVal = strings.TrimSpace(*body.Proxy)
			}
			updated, err = rt.Admin.CreateAccount(p.ctx, api.AccountCreateInput{Proxy: proxyVal}, nil)
		}
		if err != nil {
			return jsonManagementResponse(http.StatusBadRequest, map[string]any{"error": err.Error()}), nil
		}
		if updated.Enabled {
			p.serviceStopped.Store(false)
		}
		_ = p.SyncAccountToHostAuth(req.HostCallbackID, updated)
		go func() { _ = rt.EnsureRunning(p.ctx) }()
		return jsonManagementResponse(http.StatusOK, map[string]any{"account": redactAdminAccount(updated)}), nil
	case method == http.MethodPost && path == "/v0/management/aistudio2api/accounts/discover-cookies":
		var input api.CookieImportInput
		if err := json.Unmarshal(req.Body, &input); err != nil {
			return jsonManagementResponse(http.StatusBadRequest, map[string]any{"error": err.Error()}), nil
		}
		discovered, err := rt.Admin.DiscoverCookieAccounts(p.ctx, input)
		if err != nil {
			return jsonManagementResponse(http.StatusBadRequest, map[string]any{"error": err.Error()}), nil
		}
		return jsonManagementResponse(http.StatusOK, map[string]any{"accounts": discovered}), nil

	case method == http.MethodPost && path == "/v0/management/aistudio2api/accounts":
		var body struct {
			Label        string                 `json:"label"`
			Email        string                 `json:"email"`
			AuthUser     int                    `json:"auth_user"`
			Proxy        string                 `json:"proxy"`
			Locale       string                 `json:"locale"`
			Timezone     string                 `json:"timezone"`
			Cookies      string                 `json:"cookies"`
			StorageState *aistudio.StorageState `json:"storage_state"`
		}
		if err := json.Unmarshal(req.Body, &body); err != nil {
			return jsonManagementResponse(http.StatusBadRequest, map[string]any{"error": "invalid JSON body: " + err.Error()}), nil
		}
		var state aistudio.StorageState
		if body.StorageState != nil && len(body.StorageState.Cookies) > 0 {
			state = *body.StorageState
		} else if strings.TrimSpace(body.Cookies) != "" {
			parsed, err := aistudio.ParseCookieText(body.Cookies)
			if err != nil {
				return jsonManagementResponse(http.StatusBadRequest, map[string]any{"error": "failed to parse cookies: " + err.Error()}), nil
			}
			state = parsed
		} else {
			return jsonManagementResponse(http.StatusBadRequest, map[string]any{"error": "cookies or storage_state is required"}), nil
		}

		label := strings.TrimSpace(body.Label)
		if label == "" {
			label = strings.TrimSpace(body.Email)
		}
		created, err := rt.Admin.ImportAccountState(p.ctx, api.AccountStateInput{
			Label:        label,
			AuthUser:     body.AuthUser,
			Proxy:        body.Proxy,
			Locale:       body.Locale,
			Timezone:     body.Timezone,
			StorageState: state,
		})
		if err != nil {
			return jsonManagementResponse(http.StatusBadRequest, map[string]any{"error": err.Error()}), nil
		}
		if created.Enabled {
			p.serviceStopped.Store(false)
		}
		_ = p.SyncAccountToHostAuth(req.HostCallbackID, created)
		go func() { _ = rt.EnsureRunning(p.ctx) }()
		return jsonManagementResponse(http.StatusCreated, map[string]any{"account": redactAdminAccount(created)}), nil

	case method == http.MethodPost && path == "/v0/management/aistudio2api/accounts/refresh":
		var body struct {
			AccountID string `json:"account_id"`
		}
		_ = json.Unmarshal(req.Body, &body)
		if strings.TrimSpace(body.AccountID) == "" {
			return jsonManagementResponse(http.StatusBadRequest, map[string]any{"error": "account_id is required"}), nil
		}
		models, err := rt.RefreshAccountModels(p.ctx, body.AccountID)
		if err != nil {
			return jsonManagementResponse(http.StatusBadGateway, map[string]any{"error": err.Error()}), nil
		}
		if accounts, err := rt.Admin.Accounts(p.ctx); err == nil {
			for _, acc := range accounts {
				if strings.EqualFold(acc.ID, body.AccountID) {
					_ = p.SyncAccountToHostAuth(req.HostCallbackID, acc)
				}
			}
		}
		return jsonManagementResponse(http.StatusOK, map[string]any{"models_count": len(models), "models": models}), nil

	case method == http.MethodPost && path == "/v0/management/aistudio2api/accounts/update":
		var body struct {
			AccountID string  `json:"account_id"`
			Enabled   bool    `json:"enabled"`
			Proxy     *string `json:"proxy,omitempty"`
			Locale    string  `json:"locale"`
			Timezone  string  `json:"timezone"`
		}
		if err := json.Unmarshal(req.Body, &body); err != nil {
			return jsonManagementResponse(http.StatusBadRequest, map[string]any{"error": err.Error()}), nil
		}
		locale := strings.TrimSpace(body.Locale)
		timezone := strings.TrimSpace(body.Timezone)
		existingProxy := ""
		if existing, err := rt.Admin.Accounts(p.ctx); err == nil {
			for _, acc := range existing {
				if strings.EqualFold(acc.ID, body.AccountID) {
					existingProxy = acc.Proxy
					if locale == "" {
						locale = acc.Locale
					}
					if timezone == "" {
						timezone = acc.Timezone
					}
					break
				}
			}
		}
		if locale == "" {
			locale = "en-US"
		}
		if timezone == "" {
			timezone = "America/Los_Angeles"
		}
		resolvedProxy, _ := resolveUpdatedProxy(body.Proxy, existingProxy)
		updated, err := rt.Admin.UpdateAccount(p.ctx, body.AccountID, api.AccountInput{
			Label:    body.AccountID,
			Enabled:  body.Enabled,
			Proxy:    resolvedProxy,
			Locale:   locale,
			Timezone: timezone,
		})
		if err != nil {
			return jsonManagementResponse(http.StatusBadRequest, map[string]any{"error": err.Error()}), nil
		}
		_ = p.SyncAccountToHostAuth(req.HostCallbackID, updated)
		return jsonManagementResponse(http.StatusOK, map[string]any{"account": redactAdminAccount(updated)}), nil
	case method == http.MethodPost && path == "/v0/management/aistudio2api/accounts/delete":
		var body struct {
			AccountID string `json:"account_id"`
		}
		if err := json.Unmarshal(req.Body, &body); err != nil {
			return jsonManagementResponse(http.StatusBadRequest, map[string]any{"error": err.Error()}), nil
		}
		if err := rt.Admin.DeleteAccount(p.ctx, body.AccountID); err != nil {
			return jsonManagementResponse(http.StatusBadRequest, map[string]any{"error": err.Error()}), nil
		}
		p.RemoveAccountFromHostAuth(body.AccountID)
		return jsonManagementResponse(http.StatusOK, map[string]any{"deleted": body.AccountID}), nil

	case method == http.MethodPost && path == "/v0/management/aistudio2api/accounts/sync-auth":
		accounts, err := rt.Admin.Accounts(p.ctx)
		if err != nil {
			return jsonManagementResponse(http.StatusInternalServerError, map[string]any{"error": err.Error()}), nil
		}
		synced := 0
		var syncErrors []string
		for _, acc := range accounts {
			if err := p.SyncAccountToHostAuth(req.HostCallbackID, acc); err != nil {
				syncErrors = append(syncErrors, fmt.Sprintf("%s: %v", acc.ID, err))
			} else {
				synced++
			}
		}
		return jsonManagementResponse(http.StatusOK, map[string]any{
			"synced": synced,
			"errors": syncErrors,
		}), nil

	case method == http.MethodPost && path == "/v0/management/aistudio2api/cooldowns/clear":
		var body struct {
			AccountID string `json:"account_id"`
			Scope     string `json:"scope"`
		}
		_ = json.Unmarshal(req.Body, &body)
		accountID := strings.TrimSpace(body.AccountID)
		scope := strings.TrimSpace(body.Scope)
		if accountID == "" {
			return jsonManagementResponse(http.StatusBadRequest, map[string]any{"error": "请指定要清除冷却的账户邮箱 (account_id)"}), nil
		}
		if err := rt.Admin.ClearCooldown(p.ctx, accountID, scope); err != nil {
			return jsonManagementResponse(http.StatusBadRequest, map[string]any{"error": err.Error()}), nil
		}
		return jsonManagementResponse(http.StatusOK, map[string]any{"cleared": true, "account_id": accountID}), nil
	case method == http.MethodGet && path == "/v0/management/aistudio2api/models":
		models, err := rt.Admin.CatalogModels(p.ctx)
		if err != nil || len(models) == 0 {
			models = p.collectRawCatalogModels("")
		}
		return jsonManagementResponse(http.StatusOK, map[string]any{"models": models}), nil

	case method == http.MethodGet && path == "/v0/management/aistudio2api/logs":
		logs := rt.RecentLogs()
		return jsonManagementResponse(http.StatusOK, map[string]any{"logs": logs}), nil

	default:
		return jsonManagementResponse(http.StatusNotFound, map[string]any{
			"error": fmt.Sprintf("route %s %s not found", method, path),
		}), nil
	}
}

func redactProxyURLString(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return ""
	}
	parsed, err := url.Parse(trimmed)
	if err != nil || parsed.User == nil {
		return trimmed
	}
	return parsed.Redacted()
}

func redactAdminAccount(acc api.AdminAccount) api.AdminAccount {
	acc.Proxy = redactProxyURLString(acc.Proxy)
	acc.Egress = redactProxyURLString(acc.Egress)
	return acc
}

func redactAdminAccounts(accounts []api.AdminAccount) []api.AdminAccount {
	out := make([]api.AdminAccount, len(accounts))
	for i := range accounts {
		out[i] = redactAdminAccount(accounts[i])
	}
	return out
}

func resolveUpdatedProxy(newProxyPtr *string, existingProxy string) (string, bool) {
	cur := strings.TrimSpace(existingProxy)
	if newProxyPtr == nil {
		return cur, false
	}
	candidate := strings.TrimSpace(*newProxyPtr)
	if candidate == "" {
		return "", cur != ""
	}
	if cur != "" && candidate == redactProxyURLString(cur) {
		return cur, false
	}
	return candidate, candidate != cur
}

func (p *Plugin) buildStatusManagementResponse(rt *app.PluginRuntime) pluginapi.ManagementResponse {
	status, _ := rt.Admin.Status(p.ctx)
	accounts, _ := rt.Admin.Accounts(p.ctx)
	accounts = redactAdminAccounts(accounts)
	cooldowns, _ := rt.Admin.Cooldowns(p.ctx)
	now := time.Now()
	seenCooldowns := make(map[string]struct{}, len(cooldowns))
	for _, cd := range cooldowns {
		seenCooldowns[strings.ToLower(cd.AccountID)+"|"+cd.Scope] = struct{}{}
	}
	for _, st := range rt.Pool().Status() {
		for sc, cd := range st.Cooldowns {
			if !cd.Active(now) {
				continue
			}
			key := strings.ToLower(st.ID) + "|" + sc
			if _, ok := seenCooldowns[key]; ok {
				continue
			}
			seenCooldowns[key] = struct{}{}
			modelID := sc
			channel := string(aistudio.ChannelPlayground)
			if sc == "*" {
				modelID = "*"
				channel = "all"
			} else if after, ok := strings.CutPrefix(sc, string(aistudio.ChannelBuild)+":"); ok {
				modelID = after
				channel = string(aistudio.ChannelBuild)
			} else if rt.Pool().AssistantModel(modelID) {
				channel = string(aistudio.ChannelAssistant)
			}
			cooldowns = append(cooldowns, api.AdminCooldown{
				AccountID:    st.ID,
				AccountLabel: st.Label,
				Channel:      channel,
				ModelID:      modelID,
				Scope:        sc,
				Until:        cd.Until,
				Reason:       cd.Reason,
			})
		}
	}
	activeReqs, _ := rt.Admin.Requests(p.ctx)
	models, _ := rt.Admin.CatalogModels(p.ctx)
	chromeBin, _ := aistudio.FindLocalChromeBinary()
	return jsonManagementResponse(http.StatusOK, map[string]any{
		"status":           status,
		"accounts":         accounts,
		"cooldowns":        cooldowns,
		"active_requests":  activeReqs,
		"models_count":     len(models),
		"waa_backend":      p.appCfg.WAABackend,
		"data_dir":         p.pluginCfg.DataDir,
		"accounts_dir":     p.appCfg.AuthStates,
		"chrome_available": chromeBin != "",
		"chrome_binary":    chromeBin,
		"enable_assistant_models": p.appCfg.EnableAssistantModels,
	})
}

func jsonManagementResponse(status int, payload any) pluginapi.ManagementResponse {
	raw, _ := json.Marshal(payload)
	return pluginapi.ManagementResponse{
		StatusCode: status,
		Headers: http.Header{
			"Content-Type": []string{"application/json; charset=utf-8"},
		},
		Body: raw,
	}
}
