package cpa

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/api"
	"github.com/Mag1cFall/AIStudio2API/internal/app"
	"github.com/Mag1cFall/AIStudio2API/internal/pluginabi"
	"github.com/Mag1cFall/AIStudio2API/internal/pluginapi"
	"github.com/google/uuid"
)

type rpcIdentifierResponse struct {
	Identifier          string   `json:"identifier"`
	Identifiers         []string `json:"identifiers,omitempty"`
	CredentialProviders []string `json:"credential_providers,omitempty"`
}

type rpcAuthLoginStartRequest struct {
	pluginapi.AuthLoginStartRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

type rpcAuthLoginPollRequest struct {
	pluginapi.AuthLoginPollRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

type rpcAuthRefreshRequest struct {
	pluginapi.AuthRefreshRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

// AuthFilePayload is the JSON structure stored in CLIProxyAPI's auths/ directory for aistudio2api.
type AuthFilePayload struct {
	Type           string                 `json:"type"`
	Provider       string                 `json:"provider,omitempty"`
	AccountID      string                 `json:"account_id,omitempty"`
	Email          string                 `json:"email,omitempty"`
	Label          string                 `json:"label,omitempty"`
	Disabled       *bool                  `json:"disabled,omitempty"`
	ServiceStopped bool                   `json:"service_stopped,omitempty"`
	Mode           string                 `json:"mode,omitempty"`
	Proxy          string                 `json:"proxy,omitempty"`
	Locale         string                 `json:"locale,omitempty"`
	Timezone       string                 `json:"timezone,omitempty"`
	AuthUser       int                    `json:"auth_user,omitempty"`
	Cookies        string                 `json:"cookies,omitempty"`
	CookieHeader   string                 `json:"cookie_header,omitempty"`
	StorageState   *aistudio.StorageState `json:"storage_state,omitempty"`
	Tier           string                 `json:"tier,omitempty"`
	State          string                 `json:"state,omitempty"`
	UpdatedAt      string                 `json:"updated_at,omitempty"`
}

func isAIStudioProvider(provider string) bool {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "aistudio2api", "aistudio", "google-aistudio", "makersuite":
		return true
	default:
		return false
	}
}

func (p *Plugin) handleAuthIdentifier() (any, error) {
	return rpcIdentifierResponse{
		Identifier:          ProviderID,
		Identifiers:         []string{ProviderID},
		CredentialProviders: []string{"aistudio2api", "aistudio", "google-aistudio"},
	}, nil
}

func (p *Plugin) handleAuthParse(raw []byte) (any, error) {
	var req pluginapi.AuthParseRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, fmt.Errorf("decode auth.parse request: %w", err)
	}
	p.updateHostConfig(req.Host)

	var payload AuthFilePayload
	if len(req.RawJSON) > 0 {
		_ = json.Unmarshal(req.RawJSON, &payload)
	}

	fileName := strings.ToLower(strings.TrimSpace(req.FileName))
	baseName := strings.ToLower(filepath.Base(fileName))
	slashPath := strings.ToLower(filepath.ToSlash(req.Path))
	switch baseName {
	case "account.json", "storage-state.json", "runtime-state.json", "camoufox-fingerprint.json", "catalog.json":
		return pluginapi.AuthParseResponse{Handled: false}, nil
	}
	if strings.Contains(slashPath, "/accounts/") || strings.Contains(slashPath, "/chrome-cache/") || strings.Contains(filepath.ToSlash(fileName), "/") {
		return pluginapi.AuthParseResponse{Handled: false}, nil
	}

	providerMatched := isAIStudioProvider(payload.Type) ||
		isAIStudioProvider(payload.Provider) ||
		strings.HasPrefix(baseName, "aistudio2api-") ||
		strings.HasPrefix(baseName, "aistudio-")

	if !providerMatched {
		return pluginapi.AuthParseResponse{Handled: false}, nil
	}

	authData, err := p.parseAndSyncAuthPayload(p.ctx, req.FileName, req.RawJSON, payload)
	if err != nil {
		return nil, err
	}
	return pluginapi.AuthParseResponse{
		Handled: true,
		Auth:    authData,
	}, nil
}

func (p *Plugin) parseAndSyncAuthPayload(ctx context.Context, fileName string, rawJSON []byte, payload AuthFilePayload) (pluginapi.AuthData, error) {
	accountID := strings.ToLower(strings.TrimSpace(payload.AccountID))
	if accountID == "" {
		accountID = strings.ToLower(strings.TrimSpace(payload.Email))
	}
	if accountID == "" {
		accountID = strings.ToLower(strings.TrimSpace(payload.Label))
	}

	serviceStopped := p.serviceStopped.Load()
	inStartupWindow := false
	if regNs := p.lastRegisterNs.Load(); regNs > 0 && time.Since(time.Unix(0, regNs)) < 15*time.Second {
		inStartupWindow = true
	}

	prevDisabled := payload.Disabled
	prevServiceStopped := payload.ServiceStopped
	state, hasState := extractStorageStateFromPayload(rawJSON, payload)
	rt, rtErr := p.Runtime()
	var imported api.AdminAccount
	if rtErr == nil && hasState {
		acc, err := p.upsertAccountInRuntime(ctx, rt, accountID, payload, state, false)
		if err == nil && acc.ID != "" {
			imported = acc
			accountID = strings.ToLower(strings.TrimSpace(imported.ID))
			payload.AccountID = accountID
			payload.Email = accountID
			payload.Label = imported.Label
			payload.Tier = string(imported.BenefitTier)
			payload.State = string(imported.State)
		}
	}

	if accountID == "" {
		base := strings.TrimSuffix(filepath.Base(fileName), filepath.Ext(fileName))
		base = strings.TrimPrefix(base, "aistudio2api-")
		base = strings.TrimPrefix(base, "aistudio-")
		if strings.Contains(base, "@") {
			accountID = strings.ToLower(strings.TrimSpace(base))
		}
	}

	disabled := false
	if serviceStopped {
		disabled = true
	} else if prevServiceStopped || inStartupWindow {
		if rtErr == nil && imported.ID != "" {
			disabled = !imported.Enabled
		}
	} else if prevDisabled != nil {
		disabled = *prevDisabled
		if rtErr == nil && imported.ID != "" {
			wantEnabled := !disabled
			if imported.Enabled != wantEnabled {
				locale := imported.Locale
				if locale == "" {
					locale = "en-US"
				}
				tz := imported.Timezone
				if tz == "" {
					tz = "America/Los_Angeles"
				}
				if updated, uErr := rt.Admin.UpdateAccount(ctx, imported.ID, api.AccountInput{
					Label:    imported.ID,
					Enabled:  wantEnabled,
					Proxy:    imported.Proxy,
					Locale:   locale,
					Timezone: tz,
				}); uErr == nil {
					imported = updated
				}
				if wantEnabled {
					go func() { _ = rt.EnsureRunning(p.ctx) }()
				}
			}
		}
	} else if rtErr == nil && imported.ID != "" {
		disabled = !imported.Enabled
	}

	label := strings.TrimSpace(payload.Label)
	if label == "" {
		label = accountID
	}
	if label == "" {
		label = "aistudio2api-pool"
	}

	authID := fileName
	if authID == "" {
		if accountID != "" {
			authID = "aistudio2api-" + accountID + ".json"
		} else {
			authID = "aistudio2api-pool.json"
		}
	}

	payload.Type = ProviderID
	payload.Provider = ProviderID
	payload.AccountID = accountID
	payload.Label = label
	payload.Disabled = &disabled
	payload.ServiceStopped = serviceStopped
	if hasState && payload.StorageState == nil {
		payload.StorageState = &state
	}
	if payload.UpdatedAt == "" {
		payload.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	}
	storageBytes, metadata, prefix, proxyURL := mergeAuthPayloadAndMetadata(payload, nil, nil, rawJSON)
	p.setAccountExcludedModels(accountID, extractExcludedModelsList(metadata, nil))

	attributes := map[string]string{
		"provider":   ProviderID,
		"account_id": accountID,
	}
	if payload.Tier != "" {
		attributes["tier"] = payload.Tier
	}

	authData := pluginapi.AuthData{
		ID:               authID,
		Provider:         ProviderID,
		FileName:         fileName,
		Label:            label,
		Prefix:           prefix,
		Disabled:         disabled,
		ProxyURL:         proxyURL,
		StorageJSON:      storageBytes,
		Metadata:         metadata,
		Attributes:       attributes,
		NextRefreshAfter: time.Now().Add(30 * time.Minute),
	}
	if boolOrDefault(p.Config().SyncAuthFiles, true) && fileName != "" && len(p.readExistingHostAuthFile(fileName)) > 0 && (prevDisabled == nil || *prevDisabled != disabled || prevServiceStopped != serviceStopped) {
		_ = p.writeAuthDataToHostDir(authData)
	}
	return authData, nil
}

func extractStorageStateFromPayload(rawJSON []byte, payload AuthFilePayload) (aistudio.StorageState, bool) {
	if payload.StorageState != nil && len(payload.StorageState.Cookies) > 0 {
		if err := payload.StorageState.Validate(); err == nil {
			return *payload.StorageState, true
		}
	}
	cookieSource := strings.TrimSpace(payload.Cookies)
	if cookieSource == "" {
		cookieSource = strings.TrimSpace(payload.CookieHeader)
	}
	if cookieSource != "" {
		if state, err := aistudio.ParseCookieText(cookieSource); err == nil && len(state.Cookies) > 0 {
			return state, true
		}
	}
	if len(rawJSON) > 0 {
		if state, err := aistudio.ParseCookieText(string(rawJSON)); err == nil && len(state.Cookies) > 0 {
			return state, true
		}
	}
	return aistudio.StorageState{}, false
}

func (p *Plugin) upsertAccountInRuntime(
	ctx context.Context,
	rt *app.PluginRuntime,
	accountID string,
	payload AuthFilePayload,
	state aistudio.StorageState,
	forceUpdate bool,
) (api.AdminAccount, error) {
	if err := state.Validate(); err != nil {
		return api.AdminAccount{}, err
	}
	if accountID != "" && !forceUpdate {
		if existing, ok := rt.Pool().AccountByID(accountID); ok && existing != nil && existing.State != aistudio.AccountAuthRequired {
			accounts, _ := rt.Admin.Accounts(ctx)
			for _, acc := range accounts {
				if strings.EqualFold(acc.ID, accountID) {
					return acc, nil
				}
			}
		}
	}
	created, err := rt.Admin.ImportAccountState(ctx, api.AccountStateInput{
		Label:        accountID,
		AuthUser:     payload.AuthUser,
		Proxy:        payload.Proxy,
		Locale:       payload.Locale,
		Timezone:     payload.Timezone,
		StorageState: state,
	})
	if err != nil {
		accounts, _ := rt.Admin.Accounts(ctx)
		for _, acc := range accounts {
			if accountID != "" && strings.EqualFold(acc.ID, accountID) {
				return acc, nil
			}
		}
		return api.AdminAccount{}, err
	}
	return created, nil
}

func resolveHostOrigin(baseURL string) string {
	trimmed := strings.TrimSpace(baseURL)
	if trimmed != "" {
		if parsed, err := url.Parse(trimmed); err == nil && (parsed.Scheme == "http" || parsed.Scheme == "https") && parsed.Host != "" {
			return parsed.Scheme + "://" + parsed.Host
		}
	}
	return "http://127.0.0.1:8317"
}

func (p *Plugin) handleAuthLoginStart(raw []byte) (any, error) {
	var req rpcAuthLoginStartRequest
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &req)
	}
	p.updateHostConfig(req.Host)
	rt, err := p.Runtime()
	if err != nil {
		return nil, err
	}
	initial := make(map[string]struct{})
	if accounts, err := rt.Admin.Accounts(p.ctx); err == nil {
		for _, acc := range accounts {
			initial[strings.ToLower(strings.TrimSpace(acc.ID))] = struct{}{}
		}
	}
	stateID := uuid.NewString()
	session := &loginSessionState{
		CreatedAt:       time.Now(),
		InitialAccounts: initial,
		Provider:        ProviderID,
	}
	now := time.Now()
	p.loginStateMu.Lock()
	for k, v := range p.loginStates {
		if v == nil || now.Sub(v.CreatedAt) > 15*time.Minute {
			delete(p.loginStates, k)
		}
	}
	p.loginStates[stateID] = session
	p.loginStateMu.Unlock()

	proxyURL := strings.TrimSpace(p.appCfg.Proxy)
	if proxyURL == "" {
		proxyURL = strings.TrimSpace(req.Host.ProxyURL)
	}
	hostCallbackID := req.HostCallbackID

	// Immediately launch the local Chrome Incognito window in the background when the user clicks "Start Login" in ECPA
	if os.Getenv("AISTUDIO2API_NO_AUTO_BROWSER") == "" {
		if chromeBin, _ := aistudio.FindLocalChromeBinary(); chromeBin != "" && p.chromeLogin.TryLock() {
			go func(state string, proxy string, cbID string) {
				defer p.chromeLogin.Unlock()
				defer func() {
					if r := recover(); r != nil {
						p.loginStateMu.Lock()
						if st, ok := p.loginStates[state]; ok && st != nil {
							st.Done = true
							st.ErrorMessage = fmt.Sprintf("login panic: %v", r)
						}
						p.loginStateMu.Unlock()
					}
				}()
				created, loginErr := rt.Admin.CreateAccount(p.ctx, api.AccountCreateInput{
					Proxy: proxy,
				}, nil)

				p.loginStateMu.Lock()
				st, ok := p.loginStates[state]
				if ok && st != nil {
					st.Done = true
					if loginErr != nil {
						st.ErrorMessage = loginErr.Error()
					} else {
						accCopy := created
						st.Account = &accCopy
					}
				}
				p.loginStateMu.Unlock()

				if loginErr == nil {
					_ = p.SyncAccountToHostAuth(cbID, created)
					go func() { _ = rt.EnsureRunning(p.ctx) }()
				}
			}(stateID, proxyURL, hostCallbackID)
		}
	}

	origin := resolveHostOrigin(req.BaseURL)
	loginURL := origin + "/v0/resource/plugins/aistudio2api/accounts"
	return pluginapi.AuthLoginStartResponse{
		Provider:  ProviderID,
		URL:       loginURL,
		State:     stateID,
		ExpiresAt: time.Now().Add(10 * time.Minute),
		Metadata: map[string]any{
			"instructions": "Local Chrome Incognito window has been launched for Google AI Studio login. Complete sign-in there, or open " + loginURL + " to manage accounts.",
			"resource_url": loginURL,
		},
	}, nil
}

func (p *Plugin) handleAuthLoginPoll(raw []byte) (any, error) {
	var req rpcAuthLoginPollRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	p.updateHostConfig(req.Host)
	rt, err := p.Runtime()
	if err != nil {
		return nil, err
	}
	stateKey := strings.TrimSpace(req.State)
	p.loginStateMu.Lock()
	session, exists := p.loginStates[stateKey]
	var doneAccount *api.AdminAccount
	var doneErr string
	var initial map[string]struct{}
	if exists && session != nil {
		initial = session.InitialAccounts
		if session.Done {
			doneAccount = session.Account
			doneErr = session.ErrorMessage
		}
	}
	p.loginStateMu.Unlock()

	if doneAccount != nil {
		if authData, err := p.buildAuthDataForAccount(rt, *doneAccount); err == nil {
			p.loginStateMu.Lock()
			delete(p.loginStates, stateKey)
			p.loginStateMu.Unlock()
			return pluginapi.AuthLoginPollResponse{
				Status:  "success",
				Message: "AI Studio account logged in via Chrome Incognito: " + doneAccount.Label,
				Auth:    authData,
			}, nil
		}
	}

	accounts, err := rt.Admin.Accounts(p.ctx)
	if err != nil {
		return nil, err
	}
	for _, acc := range accounts {
		id := strings.ToLower(strings.TrimSpace(acc.ID))
		if exists && initial != nil {
			if _, wasPresent := initial[id]; wasPresent {
				continue
			}
		}
		authData, err := p.buildAuthDataForAccount(rt, acc)
		if err != nil {
			continue
		}
		p.loginStateMu.Lock()
		delete(p.loginStates, stateKey)
		p.loginStateMu.Unlock()
		return pluginapi.AuthLoginPollResponse{
			Status:  "success",
			Message: "AI Studio account imported: " + acc.Label,
			Auth:    authData,
		}, nil
	}

	if doneErr != "" {
		p.loginStateMu.Lock()
		delete(p.loginStates, stateKey)
		p.loginStateMu.Unlock()
		return pluginapi.AuthLoginPollResponse{
			Status:  "error",
			Message: doneErr,
		}, nil
	}

	return pluginapi.AuthLoginPollResponse{
		Status:  "pending",
		Message: "Waiting for Google AI Studio login in the local Chrome Incognito window...",
	}, nil
}

func (p *Plugin) handleAuthRefresh(raw []byte) (any, error) {
	var req rpcAuthRefreshRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	p.updateHostConfig(req.Host)
	rt, err := p.Runtime()
	if err != nil {
		return nil, err
	}

	accountID := extractAccountIDFromAuth(req.StorageJSON, req.Metadata, req.Attributes)
	if accountID == "" {
		accounts, _ := rt.Admin.Accounts(p.ctx)
		if len(accounts) > 0 {
			accountID = accounts[0].ID
		}
	}
	if accountID == "" {
		return pluginapi.AuthRefreshResponse{
			Auth: pluginapi.AuthData{
				ID:               req.AuthID,
				Provider:         ProviderID,
				StorageJSON:      req.StorageJSON,
				Metadata:         req.Metadata,
				Attributes:       req.Attributes,
				NextRefreshAfter: time.Now().Add(30 * time.Minute),
			},
			NextRefreshAfter: time.Now().Add(30 * time.Minute),
		}, nil
	}

	_, _ = rt.RefreshAccountModels(p.ctx, accountID)
	accounts, _ := rt.Admin.Accounts(p.ctx)
	for _, acc := range accounts {
		if strings.EqualFold(acc.ID, accountID) {
			authData, err := p.buildAuthDataForAccountWithExisting(rt, acc, req.StorageJSON, req.Metadata, req.Attributes)
			if err == nil {
				if req.AuthID != "" {
					authData.ID = req.AuthID
				}
				return pluginapi.AuthRefreshResponse{
					Auth:             authData,
					NextRefreshAfter: authData.NextRefreshAfter,
				}, nil
			}
		}
	}

	return pluginapi.AuthRefreshResponse{
		Auth: pluginapi.AuthData{
			ID:               req.AuthID,
			Provider:         ProviderID,
			StorageJSON:      req.StorageJSON,
			Metadata:         req.Metadata,
			Attributes:       req.Attributes,
			NextRefreshAfter: time.Now().Add(30 * time.Minute),
		},
		NextRefreshAfter: time.Now().Add(30 * time.Minute),
	}, nil
}

func sameCoreAuthCookies(a, b aistudio.StorageState) bool {
	const origin = "https://aistudio.google.com"
	for _, name := range []string{"SAPISID", "__Secure-1PSID", "__Secure-3PSID", "SID"} {
		va, oka := a.CookieValue(name, origin, time.Time{})
		vb, okb := b.CookieValue(name, origin, time.Time{})
		if oka != okb || va != vb {
			return false
		}
	}
	v, ok := a.CookieValue("SAPISID", origin, time.Time{})
	return ok && v != ""
}

func (p *Plugin) buildAuthDataForAccount(rt *app.PluginRuntime, acc api.AdminAccount) (pluginapi.AuthData, error) {
	return p.buildAuthDataForAccountWithExisting(rt, acc, nil, nil, nil)
}

func (p *Plugin) buildAuthDataForAccountWithExisting(
	rt *app.PluginRuntime,
	acc api.AdminAccount,
	existingJSON []byte,
	existingMeta map[string]any,
	existingAttrs map[string]string,
) (pluginapi.AuthData, error) {
	accountID := strings.ToLower(strings.TrimSpace(acc.ID))
	fileName := fmt.Sprintf("aistudio2api-%s.json", sanitizeFileComponent(accountID))
	serviceStopped := p.serviceStopped.Load()
	disabled := !acc.Enabled || acc.State == string(aistudio.AccountRestricted) || serviceStopped

	payload := AuthFilePayload{
		Type:           ProviderID,
		Provider:       ProviderID,
		AccountID:      accountID,
		Email:          accountID,
		Label:          acc.Label,
		Disabled:       &disabled,
		ServiceStopped: serviceStopped,
		Proxy:          acc.Proxy,
		Locale:         acc.Locale,
		Timezone:       acc.Timezone,
		Tier:           string(acc.BenefitTier),
		State:          string(acc.State),
		UpdatedAt:      time.Now().UTC().Format(time.RFC3339),
	}

	if poolAcc, ok := rt.Pool().AccountByID(accountID); ok && poolAcc != nil && poolAcc.Directory != "" {
		statePath := filepath.Join(poolAcc.Directory, "storage-state.json")
		if rawState, err := os.ReadFile(statePath); err == nil {
			var st aistudio.StorageState
			if json.Unmarshal(rawState, &st) == nil && len(st.Cookies) > 0 {
				payload.StorageState = &st
			}
		}
	}

	var diskBytes []byte
	if boolOrDefault(p.Config().SyncAuthFiles, true) {
		diskBytes = p.readExistingHostAuthFile(fileName)
	}
	storageBytes, metadata, prefix, proxyURL := mergeAuthPayloadAndMetadata(payload, existingMeta, existingAttrs, diskBytes, existingJSON)
	if len(storageBytes) == 0 {
		var err error
		storageBytes, err = json.Marshal(payload)
		if err != nil {
			return pluginapi.AuthData{}, err
		}
	}
	p.setAccountExcludedModels(accountID, extractExcludedModelsList(metadata, existingAttrs))

	attributes := map[string]string{
		"provider":   ProviderID,
		"account_id": accountID,
		"tier":       string(acc.BenefitTier),
	}
	for k, v := range existingAttrs {
		if _, exists := attributes[k]; !exists && strings.TrimSpace(v) != "" {
			attributes[k] = v
		}
	}

	return pluginapi.AuthData{
		ID:               fileName,
		Provider:         ProviderID,
		FileName:         fileName,
		Label:            acc.Label,
		Prefix:           prefix,
		Disabled:         disabled,
		ProxyURL:         proxyURL,
		StorageJSON:      storageBytes,
		NextRefreshAfter: time.Now().Add(30 * time.Minute),
		Metadata:         metadata,
		Attributes:       attributes,
	}, nil
}

func (p *Plugin) readExistingHostAuthFile(fileName string) []byte {
	if strings.TrimSpace(fileName) == "" {
		return nil
	}
	authDir := p.resolveHostAuthDir()
	if authDir == "" {
		return nil
	}
	target := filepath.Join(authDir, filepath.Base(fileName))
	raw, err := os.ReadFile(target)
	if err != nil {
		return nil
	}
	return raw
}

func mergeAuthPayloadAndMetadata(
	payload AuthFilePayload,
	existingMeta map[string]any,
	existingAttrs map[string]string,
	rawSources ...[]byte,
) ([]byte, map[string]any, string, string) {
	merged := make(map[string]any)
	for _, src := range rawSources {
		if len(src) == 0 {
			continue
		}
		var m map[string]any
		if json.Unmarshal(src, &m) == nil {
			for k, v := range m {
				merged[k] = v
			}
		}
	}
	for k, v := range existingMeta {
		merged[k] = v
	}

	baseBytes, err := json.Marshal(payload)
	if err == nil {
		var baseMap map[string]any
		if json.Unmarshal(baseBytes, &baseMap) == nil {
			for k, v := range baseMap {
				merged[k] = v
			}
		}
	}
	if payload.Disabled != nil {
		merged["disabled"] = *payload.Disabled
	}
	if payload.ServiceStopped {
		merged["service_stopped"] = true
	} else {
		delete(merged, "service_stopped")
	}

	prefix, _ := merged["prefix"].(string)
	prefix = strings.Trim(strings.TrimSpace(prefix), "/")

	proxyURL := strings.TrimSpace(payload.Proxy)
	if proxyURL == "" {
		if p, ok := merged["proxy_url"].(string); ok && strings.TrimSpace(p) != "" {
			proxyURL = strings.TrimSpace(p)
		} else if p, ok := merged["proxy-url"].(string); ok && strings.TrimSpace(p) != "" {
			proxyURL = strings.TrimSpace(p)
		}
	}

	if _, hasExcl := merged["excluded_models"]; !hasExcl {
		if _, hasDash := merged["excluded-models"]; !hasDash && existingAttrs != nil {
			if attrExcl := strings.TrimSpace(existingAttrs["excluded_models"]); attrExcl != "" {
				parts := strings.Split(attrExcl, ",")
				cleaned := make([]string, 0, len(parts))
				for _, part := range parts {
					if s := strings.TrimSpace(part); s != "" {
						cleaned = append(cleaned, s)
					}
				}
				if len(cleaned) > 0 {
					merged["excluded_models"] = cleaned
				}
			}
		}
	}

	storageBytes, err := json.Marshal(merged)
	if err != nil {
		storageBytes = baseBytes
	}

	metadata := make(map[string]any, len(merged))
	for k, v := range merged {
		switch k {
		case "storage_state", "cookies", "cookie_header":
			continue
		default:
			metadata[k] = v
		}
	}
	return storageBytes, metadata, prefix, proxyURL
}

func (p *Plugin) SyncAccountToHostAuth(callbackID string, acc api.AdminAccount) error {
	if !boolOrDefault(p.Config().SyncAuthFiles, true) {
		return nil
	}
	rt, err := p.Runtime()
	if err != nil {
		return err
	}
	authData, err := p.buildAuthDataForAccount(rt, acc)
	if err != nil {
		return err
	}
	if err := p.writeAuthDataToHostDir(authData); err == nil && strings.TrimSpace(callbackID) == "" {
		return nil
	}
	req := map[string]any{
		"host_callback_id": strings.TrimSpace(callbackID),
		"name":             authData.FileName,
		"json":             json.RawMessage(authData.StorageJSON),
		"metadata":         authData.Metadata,
		"attributes":       authData.Attributes,
	}
	if _, err := p.callHost(pluginabi.MethodHostAuthSave, req); err != nil {
		return p.writeAuthDataToHostDir(authData)
	}
	return nil
}

func (p *Plugin) writeAuthDataToHostDir(authData pluginapi.AuthData) error {
	authDir := p.resolveHostAuthDir()
	if authDir == "" {
		return fmt.Errorf("host auth dir is empty")
	}
	if info, err := os.Stat(authDir); err != nil || !info.IsDir() {
		return fmt.Errorf("host auth dir %s not found", authDir)
	}
	target := filepath.Join(authDir, filepath.Base(authData.FileName))
	return writeFileAtomically(target, authData.StorageJSON, 0o600)
}

func writeFileAtomically(target string, data []byte, perm os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(target), ".aistudio2api-*.tmp")
	if err != nil {
		return os.WriteFile(target, data, perm)
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	if err := os.Rename(tmpPath, target); err != nil {
		_ = os.Remove(tmpPath)
		return os.WriteFile(target, data, perm)
	}
	return nil
}

func (p *Plugin) SyncAllAccountsToHostDir(rt *app.PluginRuntime) {
	if rt == nil {
		return
	}
	accounts, err := rt.Admin.Accounts(p.ctx)
	if err != nil {
		return
	}
	for _, acc := range accounts {
		if authData, err := p.buildAuthDataForAccount(rt, acc); err == nil {
			_ = p.writeAuthDataToHostDir(authData)
		}
	}
}

func (p *Plugin) setHostAuthFilesDisabled(rt *app.PluginRuntime, forceDisabled bool) {
	enabledByAccount := make(map[string]bool)
	if rt != nil {
		if accounts, err := rt.Admin.Accounts(p.ctx); err == nil {
			for _, acc := range accounts {
				accKey := strings.ToLower(strings.TrimSpace(acc.ID))
				enabledByAccount[accKey] = acc.Enabled
				if !forceDisabled {
					fileName := fmt.Sprintf("aistudio2api-%s.json", sanitizeFileComponent(accKey))
					if len(p.readExistingHostAuthFile(fileName)) == 0 {
						if authData, bErr := p.buildAuthDataForAccount(rt, acc); bErr == nil {
							_ = p.writeAuthDataToHostDir(authData)
						}
					}
				}
			}
		}
	}
	authDir := p.resolveHostAuthDir()
	if authDir == "" {
		return
	}
	entries, err := os.ReadDir(authDir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		lower := strings.ToLower(name)
		if !strings.HasSuffix(lower, ".json") || (!strings.HasPrefix(lower, "aistudio2api-") && !strings.HasPrefix(lower, "aistudio-")) {
			continue
		}
		fullPath := filepath.Join(authDir, name)
		raw, err := os.ReadFile(fullPath)
		if err != nil {
			continue
		}
		var doc map[string]any
		if err := json.Unmarshal(raw, &doc); err != nil || doc == nil {
			continue
		}
		accID, _ := doc["account_id"].(string)
		accID = strings.ToLower(strings.TrimSpace(accID))
		targetDisabled := forceDisabled
		if !forceDisabled {
			if accEnabled, ok := enabledByAccount[accID]; ok {
				targetDisabled = !accEnabled
			} else {
				targetDisabled = false
			}
		}
		curDisabled, hasDisabled := doc["disabled"].(bool)
		curStopped, _ := doc["service_stopped"].(bool)
		if hasDisabled && curDisabled == targetDisabled && curStopped == forceDisabled {
			continue
		}
		doc["disabled"] = targetDisabled
		if forceDisabled {
			doc["service_stopped"] = true
		} else {
			delete(doc, "service_stopped")
		}
		doc["updated_at"] = time.Now().UTC().Format(time.RFC3339)
		if updated, err := json.Marshal(doc); err == nil {
			_ = writeFileAtomically(fullPath, updated, 0o600)
		}
	}
}

func (p *Plugin) RemoveAccountFromHostAuth(accountID string) {
	authDir := p.resolveHostAuthDir()
	if authDir == "" {
		return
	}
	fileName := fmt.Sprintf("aistudio2api-%s.json", sanitizeFileComponent(strings.ToLower(strings.TrimSpace(accountID))))
	_ = os.Remove(filepath.Join(authDir, fileName))
}

func extractAccountIDFromAuth(storageJSON []byte, metadata map[string]any, attributes map[string]string) string {
	if attributes != nil {
		if id := strings.ToLower(strings.TrimSpace(attributes["account_id"])); id != "" {
			return id
		}
		if id := strings.ToLower(strings.TrimSpace(attributes["email"])); id != "" {
			return id
		}
	}
	if metadata != nil {
		if id, ok := metadata["account_id"].(string); ok && strings.TrimSpace(id) != "" {
			return strings.ToLower(strings.TrimSpace(id))
		}
		if id, ok := metadata["email"].(string); ok && strings.TrimSpace(id) != "" {
			return strings.ToLower(strings.TrimSpace(id))
		}
	}
	if len(storageJSON) > 0 {
		var payload AuthFilePayload
		if json.Unmarshal(storageJSON, &payload) == nil {
			if id := strings.ToLower(strings.TrimSpace(payload.AccountID)); id != "" {
				return id
			}
			if id := strings.ToLower(strings.TrimSpace(payload.Email)); id != "" {
				return id
			}
			if id := strings.ToLower(strings.TrimSpace(payload.Label)); strings.Contains(id, "@") {
				return id
			}
		}
	}
	return ""
}

func sanitizeFileComponent(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '@' || r == '.' || r == '-' || r == '_' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	if b.Len() == 0 {
		return "account"
	}
	return b.String()
}
