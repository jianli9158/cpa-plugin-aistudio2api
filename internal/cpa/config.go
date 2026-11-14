package cpa

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/egress"
	"github.com/Mag1cFall/AIStudio2API/internal/config"
	"gopkg.in/yaml.v3"
)

const (
	PluginID      = "aistudio2api"
	PluginVersion = "1.2.6"
	ProviderID    = "aistudio2api"
)

// PluginConfig defines the YAML/JSON configuration under plugins.configs.aistudio2api in CLIProxyAPI's config.yaml.
type PluginConfig struct {
	DataDir                string        `json:"data_dir" yaml:"data_dir"`
	AccountsDir            string        `json:"accounts_dir" yaml:"accounts_dir"`
	Proxy                  string        `json:"proxy" yaml:"proxy"`
	WAABackend             string        `json:"waa_backend" yaml:"waa_backend"`
	WarmWorkers            *int          `json:"warm_workers" yaml:"warm_workers"`
	MaxActiveWorkers       *int          `json:"max_active_workers" yaml:"max_active_workers"`
	WarmStartupConc        *int          `json:"warm_startup_concurrency" yaml:"warm_startup_concurrency"`
	PerAccountConcurrency  *int          `json:"per_account_concurrency" yaml:"per_account_concurrency"`
	UpstreamChannels       []string      `json:"upstream_channels" yaml:"upstream_channels"`
	ModelChannels          string        `json:"model_channels" yaml:"model_channels"`
	RoutingStrategy        string        `json:"routing_strategy" yaml:"routing_strategy"`
	AccountOrder           string        `json:"account_order" yaml:"account_order"`
	MaxAccountSwitches     *int          `json:"max_account_switches" yaml:"max_account_switches"`
	ModelAccounts          string        `json:"model_accounts" yaml:"model_accounts"`
	ModelAliases           string        `json:"model_aliases" yaml:"model_aliases"`
	ModelMapping           string        `json:"model_mapping,omitempty" yaml:"model_mapping,omitempty"`
	BuildNativeNonstream   *bool         `json:"build_native_nonstream" yaml:"build_native_nonstream"`
	NonstreamKeepalive     *bool         `json:"nonstream_keepalive" yaml:"nonstream_keepalive"`
	FakeStream             *bool         `json:"fake_stream" yaml:"fake_stream"`
	StripThoughtSignatures *bool         `json:"strip_thought_signatures" yaml:"strip_thought_signatures"`
	YouTubeLinks           *bool         `json:"youtube_links" yaml:"youtube_links"`
	FirstEventHedge        string        `json:"first_event_hedge" yaml:"first_event_hedge"`
	FirstEventHedgeModels  string        `json:"first_event_hedge_models" yaml:"first_event_hedge_models"`
	SearchModelAliases     *bool         `json:"search_model_aliases" yaml:"search_model_aliases"`
	DiscoverHiddenModels   *bool         `json:"discover_hidden_models" yaml:"discover_hidden_models"`
	GoogleSearch           *bool         `json:"google_search" yaml:"google_search"`
	EnableAssistantModels  *bool         `json:"enable_assistant_models" yaml:"enable_assistant_models"`
	ContinuationRewrite    *bool         `json:"continuation_rewrite" yaml:"continuation_rewrite"`
	ContinuationPrompt     string        `json:"continuation_prompt" yaml:"continuation_prompt"`
	AutoStart              *bool         `json:"auto_start" yaml:"auto_start"`
	PinExecutorAccount     *bool         `json:"pin_executor_account" yaml:"pin_executor_account"`
	RouteUnprefixedModels  *bool         `json:"route_unprefixed_models" yaml:"route_unprefixed_models"`
	SyncAuthFiles          *bool         `json:"sync_auth_files" yaml:"sync_auth_files"`
	ModelPrefix            string        `json:"model_prefix" yaml:"model_prefix"`
	Options                *PluginConfig `json:"options,omitempty" yaml:"options,omitempty"`
}

func ParsePluginConfig(raw []byte) (PluginConfig, config.Config, error) {
	var pcfg PluginConfig
	trimmed := strings.TrimSpace(string(raw))
	if trimmed != "" && trimmed != "null" && trimmed != "{}" {
		if err := yaml.Unmarshal(raw, &pcfg); err != nil {
			_ = json.Unmarshal(raw, &pcfg)
		}
		if pcfg.Options != nil {
			opt := *pcfg.Options
			if pcfg.DataDir == "" {
				pcfg.DataDir = opt.DataDir
			}
			if pcfg.AccountsDir == "" {
				pcfg.AccountsDir = opt.AccountsDir
			}
			if pcfg.Proxy == "" {
				pcfg.Proxy = opt.Proxy
			}
			if pcfg.WAABackend == "" {
				pcfg.WAABackend = opt.WAABackend
			}
			if pcfg.WarmWorkers == nil {
				pcfg.WarmWorkers = opt.WarmWorkers
			}
			if pcfg.MaxActiveWorkers == nil {
				pcfg.MaxActiveWorkers = opt.MaxActiveWorkers
			}
			if pcfg.WarmStartupConc == nil {
				pcfg.WarmStartupConc = opt.WarmStartupConc
			}
			if pcfg.PerAccountConcurrency == nil {
				pcfg.PerAccountConcurrency = opt.PerAccountConcurrency
			}
			if len(pcfg.UpstreamChannels) == 0 {
				pcfg.UpstreamChannels = opt.UpstreamChannels
			}
			if pcfg.ModelPrefix == "" {
				pcfg.ModelPrefix = opt.ModelPrefix
			}
			if pcfg.RouteUnprefixedModels == nil {
				pcfg.RouteUnprefixedModels = opt.RouteUnprefixedModels
			}
			if pcfg.SyncAuthFiles == nil {
				pcfg.SyncAuthFiles = opt.SyncAuthFiles
			}
			if pcfg.ModelChannels == "" {
				pcfg.ModelChannels = opt.ModelChannels
			}
			if pcfg.RoutingStrategy == "" {
				pcfg.RoutingStrategy = opt.RoutingStrategy
			}
			if pcfg.AccountOrder == "" {
				pcfg.AccountOrder = opt.AccountOrder
			}
			if pcfg.MaxAccountSwitches == nil {
				pcfg.MaxAccountSwitches = opt.MaxAccountSwitches
			}
			if pcfg.ModelAccounts == "" {
				pcfg.ModelAccounts = opt.ModelAccounts
			}
			if pcfg.ModelAliases == "" {
				pcfg.ModelAliases = opt.ModelAliases
			}
			if pcfg.ModelMapping == "" {
				pcfg.ModelMapping = opt.ModelMapping
			}
			if pcfg.BuildNativeNonstream == nil {
				pcfg.BuildNativeNonstream = opt.BuildNativeNonstream
			}
			if pcfg.NonstreamKeepalive == nil {
				pcfg.NonstreamKeepalive = opt.NonstreamKeepalive
			}
			if pcfg.FakeStream == nil {
				pcfg.FakeStream = opt.FakeStream
			}
			if pcfg.StripThoughtSignatures == nil {
				pcfg.StripThoughtSignatures = opt.StripThoughtSignatures
			}
			if pcfg.YouTubeLinks == nil {
				pcfg.YouTubeLinks = opt.YouTubeLinks
			}
			if pcfg.FirstEventHedge == "" {
				pcfg.FirstEventHedge = opt.FirstEventHedge
			}
			if pcfg.FirstEventHedgeModels == "" {
				pcfg.FirstEventHedgeModels = opt.FirstEventHedgeModels
			}
			if pcfg.SearchModelAliases == nil {
				pcfg.SearchModelAliases = opt.SearchModelAliases
			}
			if pcfg.DiscoverHiddenModels == nil {
				pcfg.DiscoverHiddenModels = opt.DiscoverHiddenModels
			}
			if pcfg.GoogleSearch == nil {
				pcfg.GoogleSearch = opt.GoogleSearch
			}
			if pcfg.EnableAssistantModels == nil {
				pcfg.EnableAssistantModels = opt.EnableAssistantModels
			}
			if pcfg.ContinuationRewrite == nil {
				pcfg.ContinuationRewrite = opt.ContinuationRewrite
			}
			if pcfg.ContinuationPrompt == "" {
				pcfg.ContinuationPrompt = opt.ContinuationPrompt
			}
			if pcfg.AutoStart == nil {
				pcfg.AutoStart = opt.AutoStart
			}
			if pcfg.PinExecutorAccount == nil {
				pcfg.PinExecutorAccount = opt.PinExecutorAccount
			}
		}
	}

	dataDir := strings.TrimSpace(pcfg.DataDir)
	if dataDir == "" {
		dataDir = strings.TrimSpace(os.Getenv("AISTUDIO2API_DATA_DIR"))
	}
	if dataDir == "" {
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			dataDir = filepath.Join(home, ".aistudio2api")
		} else {
			dataDir = filepath.Join(".", "data", "aistudio2api")
		}
	}
	if strings.HasPrefix(dataDir, "~/") {
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			dataDir = filepath.Join(home, dataDir[2:])
		}
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		legacyInAuthDir := filepath.Join(home, ".cli-proxy-api", "aistudio2api")
		if filepath.Clean(dataDir) == filepath.Clean(legacyInAuthDir) {
			dataDir = filepath.Join(home, ".aistudio2api")
		}
	}
	accountsDir := strings.TrimSpace(pcfg.AccountsDir)
	if accountsDir == "" {
		accountsDir = strings.TrimSpace(os.Getenv("AISTUDIO_AUTH_STATES"))
	}
	if accountsDir == "" {
		accountsDir = filepath.Join(dataDir, "accounts")
	}
	if strings.HasPrefix(accountsDir, "~/") {
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			accountsDir = filepath.Join(home, accountsDir[2:])
		}
	}
	_ = os.MkdirAll(accountsDir, 0o700)
	egress.SetCacheDir(filepath.Join(dataDir, "geoip"))

	waaBackend := strings.ToLower(strings.TrimSpace(pcfg.WAABackend))
	if waaBackend == "" {
		waaBackend = strings.ToLower(strings.TrimSpace(os.Getenv("WAA_BACKEND")))
	}
	if waaBackend != config.WAABackendCamoufox && waaBackend != config.WAABackendSpiderMonkey {
		waaBackend = config.WAABackendSpiderMonkey
	}

	warmWorkers := 2
	if pcfg.WarmWorkers != nil && *pcfg.WarmWorkers >= 1 {
		warmWorkers = *pcfg.WarmWorkers
	}
	maxActive := 5
	if pcfg.MaxActiveWorkers != nil && *pcfg.MaxActiveWorkers >= 1 {
		maxActive = *pcfg.MaxActiveWorkers
	}
	if maxActive < warmWorkers {
		maxActive = warmWorkers
	}
	warmStartup := 2
	if pcfg.WarmStartupConc != nil && *pcfg.WarmStartupConc >= 1 {
		warmStartup = *pcfg.WarmStartupConc
	}
	perAccountConc := 2
	if pcfg.PerAccountConcurrency != nil && *pcfg.PerAccountConcurrency >= 1 {
		perAccountConc = *pcfg.PerAccountConcurrency
	}

	channels := pcfg.UpstreamChannels
	if len(channels) == 0 {
		channels = []string{"playground"}
	}

	hedge := strings.TrimSpace(pcfg.FirstEventHedge)
	if hedge == "" {
		hedge = config.FirstEventHedgeAuto
	}

	cfg := config.Default()
	cfg.AuthStates = accountsDir
	cfg.Proxy = strings.TrimSpace(pcfg.Proxy)
	cfg.WAABackend = waaBackend
	cfg.InitTimeout = 2 * time.Minute
	cfg.RequestTimeout = 10 * time.Minute
	cfg.WarmWorkerLimit = warmWorkers
	cfg.MaxActiveWorkers = maxActive
	cfg.WarmStartupConcurrency = warmStartup
	cfg.PerAccountConcurrency = perAccountConc
	cfg.UpstreamChannels = channels
	cfg.ModelChannels = strings.TrimSpace(pcfg.ModelChannels)
	if strategy := strings.TrimSpace(pcfg.RoutingStrategy); strategy == "round-robin" || strategy == "fill-first" {
		cfg.RoutingStrategy = strategy
	}
	cfg.AccountOrder = strings.TrimSpace(pcfg.AccountOrder)
	if pcfg.MaxAccountSwitches != nil && *pcfg.MaxAccountSwitches >= 0 {
		cfg.MaxAccountSwitches = *pcfg.MaxAccountSwitches
	}
	cfg.ModelAccounts = strings.TrimSpace(pcfg.ModelAccounts)
	aliasesRaw := strings.TrimSpace(pcfg.ModelAliases)
	if aliasesRaw == "" {
		aliasesRaw = strings.TrimSpace(pcfg.ModelMapping)
	}
	cfg.ModelMapping = normalizeModelAliasesString(aliasesRaw)
	cfg.BuildNativeNonstream = boolOrDefault(pcfg.BuildNativeNonstream, true)
	cfg.NonstreamKeepalive = boolOrDefault(pcfg.NonstreamKeepalive, false)
	cfg.FakeStream = boolOrDefault(pcfg.FakeStream, false)
	cfg.StripThoughtSignatures = boolOrDefault(pcfg.StripThoughtSignatures, false)
	cfg.YouTubeLinks = boolOrDefault(pcfg.YouTubeLinks, true)
	hedgeModels := strings.TrimSpace(pcfg.FirstEventHedgeModels)
	if hedgeModels == "" {
		hedgeModels = strings.TrimSpace(os.Getenv("FIRST_EVENT_HEDGE_MODELS"))
	}
	if hedgeModels == "" {
		hedgeModels = config.DefaultFirstEventHedgeModels
	}
	cfg.FirstEventHedge = hedge
	cfg.FirstEventHedgeModels = hedgeModels
	cfg.SearchModelAliases = boolOrDefault(pcfg.SearchModelAliases, true)
	cfg.DiscoverHiddenModels = boolOrDefault(pcfg.DiscoverHiddenModels, false)
	cfg.GoogleSearch = boolOrDefault(pcfg.GoogleSearch, false)
	cfg.EnableAssistantModels = boolOrDefault(pcfg.EnableAssistantModels, false)
	cfg.ContinuationRewrite = boolOrDefault(pcfg.ContinuationRewrite, true)
	if strings.TrimSpace(pcfg.ContinuationPrompt) != "" {
		cfg.ContinuationPrompt = strings.TrimSpace(pcfg.ContinuationPrompt)
	}
	cfg.AutoStart = boolOrDefault(pcfg.AutoStart, true)
	cfg.MediaOutput = config.MediaOutputInline

	pcfg.ModelPrefix = strings.TrimSpace(pcfg.ModelPrefix)
	return pcfg, cfg, nil
}

func boolOrDefault(ptr *bool, def bool) bool {
	if ptr == nil {
		return def
	}
	return *ptr
}

func normalizeModelAliasesString(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if !strings.Contains(part, ":") && strings.Contains(part, "=") {
			part = strings.Replace(part, "=", ":", 1)
		}
		out = append(out, part)
	}
	return strings.Join(out, ",")
}
