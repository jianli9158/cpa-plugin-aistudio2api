package cpa

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/pluginapi"
)

type rpcRequestCompletion struct {
	pluginapi.RequestCompletion
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

type rpcQuotaFetchRequest struct {
	pluginapi.QuotaFetchRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

type rpcQuotaResetRequest struct {
	pluginapi.QuotaResetRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

func (p *Plugin) handleSchedulerPick(raw []byte) (any, error) {
	var req pluginapi.SchedulerPickRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, fmt.Errorf("decode scheduler.pick request: %w", err)
	}
	if len(req.Candidates) == 0 {
		return pluginapi.SchedulerPickResponse{Handled: false}, nil
	}

	// Only handle scheduling when candidates belong to aistudio2api.
	aistudioCandidates := make([]pluginapi.SchedulerAuthCandidate, 0, len(req.Candidates))
	for _, cand := range req.Candidates {
		if isAIStudioProvider(cand.Provider) || isAIStudioCandidate(cand) {
			aistudioCandidates = append(aistudioCandidates, cand)
		}
	}
	if len(aistudioCandidates) == 0 {
		return pluginapi.SchedulerPickResponse{Handled: false}, nil
	}
	if p.serviceStopped.Load() {
		if len(aistudioCandidates) < len(req.Candidates) {
			return pluginapi.SchedulerPickResponse{Handled: false}, nil
		}
		return pluginapi.SchedulerPickResponse{
			Handled:      true,
			Reject:       true,
			RejectCode:   "service_stopped",
			RejectReason: "aistudio2api service is stopped",
		}, nil
	}

	rt, err := p.Runtime()
	if err != nil {
		return pluginapi.SchedulerPickResponse{
			Handled:         true,
			DelegateBuiltin: pluginapi.SchedulerBuiltinRoundRobin,
		}, nil
	}

	cleanModel := StripModelPrefix(req.Model, p.Config().ModelPrefix)
	cleanModel = strings.TrimPrefix(cleanModel, "models/")
	baseModel, _, suffixErr := aistudio.ApplyModelSuffix(cleanModel, aistudio.GenerationConfig{})
	if suffixErr == nil && baseModel != "" {
		cleanModel = baseModel
	}
	cleanModel = rt.Pool().CanonicalModelID(cleanModel)

	candidateByAccount := make(map[string]string, len(aistudioCandidates))
	var poolFallbackCandidateID string
	for _, cand := range aistudioCandidates {
		if isCandidateStatusIneligible(cand) {
			continue
		}
		accID := extractAccountIDFromAuth(nil, cand.Metadata, cand.Attributes)
		if accID == "" && strings.Contains(cand.ID, "@") {
			accID = strings.ToLower(strings.TrimSpace(cand.ID))
		}
		if accID != "" {
			candidateByAccount[accID] = cand.ID
		} else if poolFallbackCandidateID == "" {
			poolFallbackCandidateID = cand.ID
		}
	}

	upstreamMethod := inferUpstreamMethodForModel(cleanModel)
	allowedAccounts := make([]string, 0, len(candidateByAccount))
	for accID := range candidateByAccount {
		allowedAccounts = append(allowedAccounts, accID)
	}
	selection := aistudio.AccountSelection{
		ModelID:           cleanModel,
		Method:            upstreamMethod,
		AllowedAccountIDs: allowedAccounts,
	}
	groups, classifyErr := rt.Pool().ClassifyCandidates(p.ctx, selection, nil)
	if classifyErr == nil && upstreamMethod != "" {
		total := len(groups.WarmReady) + len(groups.StandbyReady) + len(groups.WarmAvailable) + len(groups.StandbyBusy) + len(groups.WarmBusy)
		if total == 0 && groups.EarliestCooldown.Until.IsZero() {
			fallbackSel := selection
			fallbackSel.Method = ""
			if fallbackGroups, fErr := rt.Pool().ClassifyCandidates(p.ctx, fallbackSel, nil); fErr == nil {
				groups = fallbackGroups
			}
		}
	}

	hostDispatched := false
	if req.Options.Metadata != nil {
		if selID, ok := req.Options.Metadata[pluginapi.SelectionIDMetadataKey].(string); ok && strings.TrimSpace(selID) != "" {
			hostDispatched = true
		}
	}

	if classifyErr == nil {
		orderedTiers := [][]string{
			groups.WarmReady,
			groups.StandbyReady,
			groups.WarmAvailable,
			groups.StandbyBusy,
			groups.WarmBusy,
		}
		var firstEligibleAuthID string
		eligibleCandidateCount := 0
		for _, tier := range orderedTiers {
			for _, accID := range tier {
				if authID, ok := candidateByAccount[strings.ToLower(strings.TrimSpace(accID))]; ok && authID != "" {
					if firstEligibleAuthID == "" {
						firstEligibleAuthID = authID
					}
					eligibleCandidateCount++
				}
			}
		}

		// Defer to the host's SessionAffinitySelector + RoundRobinSelector whenever
		// hostDispatched is true and at least one candidate account is eligible in the
		// plugin pool.
		if hostDispatched && eligibleCandidateCount > 0 {
			return pluginapi.SchedulerPickResponse{Handled: false}, nil
		}

		if firstEligibleAuthID != "" {
			return pluginapi.SchedulerPickResponse{
				Handled: true,
				AuthID:  firstEligibleAuthID,
			}, nil
		}

		// If a generic pool auth file exists and at least one pool account is eligible, pick it
		totalEligible := len(groups.WarmReady) + len(groups.StandbyReady) + len(groups.WarmAvailable) + len(groups.StandbyBusy) + len(groups.WarmBusy)
		if totalEligible > 0 && poolFallbackCandidateID != "" {
			if hostDispatched && len(candidateByAccount) == 0 {
				return pluginapi.SchedulerPickResponse{Handled: false}, nil
			}
			return pluginapi.SchedulerPickResponse{
				Handled: true,
				AuthID:  poolFallbackCandidateID,
			}, nil
		}

		// If all accounts in the pool are cooling down for this model and cooldown exceeds 60s, report 429 cooldown
		if totalEligible == 0 && !groups.EarliestCooldown.Until.IsZero() && time.Until(groups.EarliestCooldown.Until) > time.Minute {
			rejectCode := "rate_limit_exceeded"
			if strings.HasPrefix(groups.EarliestCooldown.Reason, aistudio.AccountRejectionKind) {
				rejectCode = "account_unavailable"
			}
			return pluginapi.SchedulerPickResponse{
				Handled:      true,
				Reject:       true,
				RejectCode:   rejectCode,
				RejectReason: fmt.Sprintf("all AI Studio accounts for model %s are cooling down until %s (%s)", cleanModel, groups.EarliestCooldown.Until.Format(time.RFC3339), groups.EarliestCooldown.Reason),
			}, nil
		}
	}

	if hostDispatched {
		return pluginapi.SchedulerPickResponse{Handled: false}, nil
	}

	// Fallback: match any ready/busy account from Pool.Status()
	for _, st := range rt.Pool().Status() {
		if !st.Enabled || st.State == aistudio.AccountCooldown || st.State == aistudio.AccountAuthRequired || st.State == aistudio.AccountRestricted || st.State == aistudio.AccountUnavailable {
			continue
		}
		if authID, ok := candidateByAccount[strings.ToLower(strings.TrimSpace(st.ID))]; ok && authID != "" {
			return pluginapi.SchedulerPickResponse{
				Handled: true,
				AuthID:  authID,
			}, nil
		}
	}

	if poolFallbackCandidateID != "" {
		return pluginapi.SchedulerPickResponse{
			Handled: true,
			AuthID:  poolFallbackCandidateID,
		}, nil
	}

	return pluginapi.SchedulerPickResponse{
		Handled:         true,
		DelegateBuiltin: pluginapi.SchedulerBuiltinRoundRobin,
	}, nil
}

func inferUpstreamMethodForModel(modelID string) string {
	lower := strings.ToLower(strings.TrimSpace(modelID))
	switch {
	case strings.Contains(lower, "decision"):
		return "createDecision"
	case strings.Contains(lower, "embedding"):
		return "embedContent"
	case strings.Contains(lower, "veo"):
		return "predictLongRunning"
	case strings.Contains(lower, "imagen"):
		return "predict"
	default:
		return "generateContent"
	}
}

func isAIStudioCandidate(cand pluginapi.SchedulerAuthCandidate) bool {
	if cand.Attributes != nil && isAIStudioProvider(cand.Attributes["provider"]) {
		return true
	}
	if cand.Metadata != nil {
		if p, ok := cand.Metadata["provider"].(string); ok && isAIStudioProvider(p) {
			return true
		}
		if t, ok := cand.Metadata["type"].(string); ok && isAIStudioProvider(t) {
			return true
		}
	}
	idLower := strings.ToLower(strings.TrimSpace(cand.ID))
	return strings.HasPrefix(idLower, "aistudio2api") || strings.HasPrefix(idLower, "aistudio-")
}

func isCandidateStatusIneligible(cand pluginapi.SchedulerAuthCandidate) bool {
	switch strings.ToLower(strings.TrimSpace(cand.Status)) {
	case "disabled", "unavailable", "restricted", "auth_required":
		return true
	}
	if cand.Attributes != nil {
		switch strings.ToLower(strings.TrimSpace(cand.Attributes["state"])) {
		case "restricted", "auth_required", "unavailable":
			return true
		}
	}
	if cand.Metadata != nil {
		if st, ok := cand.Metadata["state"].(string); ok {
			switch strings.ToLower(strings.TrimSpace(st)) {
			case "restricted", "auth_required", "unavailable":
				return true
			}
		}
	}
	return false
}

func (p *Plugin) handleRequestComplete(raw []byte) (any, error) {
	var req rpcRequestCompletion
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	return struct{}{}, nil
}

func (p *Plugin) handleQuotaIdentifier() (any, error) {
	return rpcIdentifierResponse{
		Identifier:          ProviderID,
		Identifiers:         nil,
		CredentialProviders: []string{"aistudio2api", "aistudio", "google-aistudio"},
	}, nil
}

func (p *Plugin) handleQuotaDescribe() (any, error) {
	return pluginapi.QuotaDescribeResponse{
		SupportedProviders: []string{"aistudio2api", "aistudio", "google-aistudio"},
		DisplayName:        "Google AI Studio (WAA)",
		SupportsReset:      true,
	}, nil
}

func (p *Plugin) handleQuotaFetch(raw []byte) (any, error) {
	var req rpcQuotaFetchRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	rt, err := p.Runtime()
	if err != nil {
		return nil, err
	}

	accID := extractAccountIDFromAuth(req.StorageJSON, req.Metadata, req.Attributes)
	statuses := rt.Pool().Status()
	var matched *aistudio.AccountStatus
	for i := range statuses {
		if accID != "" && strings.EqualFold(statuses[i].ID, accID) {
			matched = &statuses[i]
			break
		}
	}
	if matched == nil && len(statuses) > 0 {
		matched = &statuses[0]
	}

	tierName := "FREE"
	remaining := 1.0
	resetTime := ""
	desc := "全部模型就绪可用"
	modelsCount := 0.0
	now := time.Now()
	if matched != nil {
		if matched.BenefitTier != "" {
			tierName = string(matched.BenefitTier)
		}
		totalModels := len(matched.Models)
		modelsCount = float64(totalModels)

		activeCooldowns := make(map[string]aistudio.CooldownState)
		var earliestReset time.Time
		var primaryReason string
		for scope, cd := range matched.Cooldowns {
			if !cd.Until.IsZero() && now.After(cd.Until) {
				_ = rt.Admin.ClearCooldown(p.ctx, matched.ID, scope)
				continue
			}
			activeCooldowns[scope] = cd
			if earliestReset.IsZero() || (!cd.Until.IsZero() && cd.Until.Before(earliestReset)) {
				earliestReset = cd.Until
				primaryReason = cd.Reason
			}
		}

		if matched.State == aistudio.AccountAuthRequired || matched.State == aistudio.AccountRestricted || !matched.Enabled {
			remaining = 0.0
			desc = "账户需要重新登录、服务受限或已停用"
			if matched.Message != "" {
				desc = matched.Message
			}
		} else if len(activeCooldowns) > 0 {
			if !earliestReset.IsZero() {
				resetTime = earliestReset.Format(time.RFC3339)
			}
			if primaryReason != "" {
				desc = primaryReason
			}
			if _, isGlobal := activeCooldowns["*"]; isGlobal || matched.State == aistudio.AccountCooldown || (totalModels > 0 && len(activeCooldowns) >= totalModels) {
				remaining = 0.0
			} else if totalModels > 0 {
				available := totalModels - len(activeCooldowns)
				if available < 0 {
					available = 0
				}
				remaining = float64(available) / float64(totalModels)
				desc = fmt.Sprintf("%d/%d 模型可用 | %s", available, totalModels, primaryReason)
			}
		} else {
			remaining = 1.0
			desc = "全部模型就绪可用"
		}
	}
	return pluginapi.QuotaFetchResponse{
		Subscription: &pluginapi.QuotaSubscription{
			Plan:     "Google AI Studio",
			TierName: tierName,
			TierID:   strings.ToLower(tierName),
		},
		Summary: []pluginapi.QuotaMetric{
			{Key: "accounts", Label: "Total Accounts", Value: float64(len(statuses)), Format: "number"},
			{Key: "models", Label: "Available Models", Value: modelsCount, Format: "number"},
		},
		Groups: []pluginapi.QuotaGroup{
			{
				DisplayName: "AI Studio Quota",
				Buckets: []pluginapi.QuotaBucket{
					{
						Window:            "current",
						RemainingFraction: remaining,
						ResetTime:         resetTime,
						Description:       desc,
					},
				},
			},
		},
	}, nil
}

func (p *Plugin) handleQuotaReset(raw []byte) (any, error) {
	var req rpcQuotaResetRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	rt, err := p.Runtime()
	if err != nil {
		return nil, err
	}

	accID := extractAccountIDFromAuth(req.StorageJSON, req.Metadata, req.Attributes)
	scope := ""
	if accID != "" {
		if err := rt.Admin.ClearCooldown(p.ctx, accID, scope); err != nil {
			return pluginapi.QuotaResetResponse{
				Success: false,
				Message: err.Error(),
			}, nil
		}
		return pluginapi.QuotaResetResponse{
			Success: true,
			Message: "Cleared AI Studio cooldowns for account " + accID,
		}, nil
	}

	for _, st := range rt.Pool().Status() {
		_ = rt.Admin.ClearCooldown(p.ctx, st.ID, scope)
	}
	return pluginapi.QuotaResetResponse{
		Success: true,
		Message: "Cleared AI Studio cooldowns for all accounts",
	}, nil
}
