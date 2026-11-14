package app

import (
	"context"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/api"
	"github.com/Mag1cFall/AIStudio2API/internal/config"
)

type pinnedAccountContextKey struct{}

// WithPinnedAccountID attaches a specific AI Studio account ID (email) to the request context.
func WithPinnedAccountID(ctx context.Context, accountID string) context.Context {
	accountID = strings.ToLower(strings.TrimSpace(accountID))
	if accountID == "" {
		return ctx
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ctx = aistudio.ContextWithAccount(ctx, accountID)
	return context.WithValue(ctx, pinnedAccountContextKey{}, accountID)
}

// ContextWithPinnedAccountID is an alias for WithPinnedAccountID.
func ContextWithPinnedAccountID(ctx context.Context, accountID string) context.Context {
	return WithPinnedAccountID(ctx, accountID)
}

// PinnedAccountIDFromContext returns the pinned account ID from context when set.
func PinnedAccountIDFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	v, _ := ctx.Value(pinnedAccountContextKey{}).(string)
	return strings.ToLower(strings.TrimSpace(v))
}

// PluginRuntime wraps runtimeManager and api.NewHandler for CLIProxyAPI plugin execution.
type PluginRuntime struct {
	Manager *runtimeManager
	Admin   api.AdminService
	Handler http.Handler
	Config  config.Config
}

// NewPluginRuntime creates and initializes a PluginRuntime from config.Config.
func NewPluginRuntime(ctx context.Context, cfg config.Config) (*PluginRuntime, error) {
	authStates := cfg.AuthStates
	proxy := cfg.Proxy
	overrides := dataConfigOverrides{
		authStates: &authStates,
		proxy:      &proxy,
	}
	mgr, err := newRuntimeManager(ctx, "", cfg, overrides)
	if err != nil {
		return nil, err
	}
	mediaDir := ""
	if cfg.AuthStates != "" {
		firstDir := strings.TrimSpace(strings.Split(cfg.AuthStates, ",")[0])
		if firstDir != "" {
			mediaDir = filepath.Join(firstDir, ".media")
		}
	}
	handler := api.NewHandler(mgr, api.Config{
		Admin:          mgr,
		MediaDir:       mediaDir,
		MediaLinks:     mgr.mediaLinks,
		ResponseModes:  mgr.responseModes,
		SearchSettings: mgr.searchSettings,
		WaitLaunch:     mgr.WaitLaunch,
		EgressProxy:    mgr.egressProxy,
	})
	rt := &PluginRuntime{
		Manager: mgr,
		Admin:   mgr,
		Handler: handler,
		Config:  cfg,
	}
	if cfg.AutoStart {
		go func() {
			_ = rt.EnsureRunning(ctx)
		}()
	}
	return rt, nil
}

// PublicHandler returns the HTTP handler for OpenAI, Responses, Anthropic, and Gemini endpoints.
func (rt *PluginRuntime) PublicHandler() http.Handler {
	return rt.Handler
}

// Close stops and releases the runtime.
func (rt *PluginRuntime) Close() error {
	if rt == nil || rt.Manager == nil {
		return nil
	}
	return rt.Manager.Close()
}

// SetFirstEventHedge updates first-event hedge threshold and target models on trackedService.
func (service *trackedService) SetFirstEventHedge(firstEventHedge, firstEventHedgeModels string) {
	if service == nil {
		return
	}
	service.hedgeAuto, service.hedgeAfter, _ = config.ParseFirstEventHedge(firstEventHedge)
	service.hedgeModels = config.ModelList(firstEventHedgeModels)
}

// HedgeSettings returns the active first-event hedge settings on the underlying trackedService.
func (rt *PluginRuntime) HedgeSettings() (bool, time.Duration, []string) {
	if rt == nil || rt.Manager == nil {
		return false, 0, nil
	}
	rt.Manager.mu.RLock()
	cur := rt.Manager.current
	rt.Manager.mu.RUnlock()
	if cur == nil {
		return false, 0, nil
	}
	ts, ok := cur.service.(*trackedService)
	if !ok || ts == nil {
		return false, 0, nil
	}
	return ts.hedgeAuto, ts.hedgeAfter, append([]string(nil), ts.hedgeModels...)
}

// WarmWorkerTarget returns the prewarm worker target configured on the underlying worker manager.
func (rt *PluginRuntime) WarmWorkerTarget() int {
	if rt == nil || rt.Manager == nil {
		return 0
	}
	rt.Manager.mu.RLock()
	cur := rt.Manager.current
	rt.Manager.mu.RUnlock()
	if cur == nil {
		return 0
	}
	ts, ok := cur.service.(*trackedService)
	if !ok || ts == nil || ts.workers == nil {
		return 0
	}
	if target := ts.workers.warmLimit(); target > 0 {
		return target
	}
	return ts.workers.PrewarmTarget()
}

// ApplyLiveConfig applies updated routing, channel, capacity, hedge, and model mapping settings to an active PluginRuntime without dropping active requests or warm workers.
func (rt *PluginRuntime) ApplyLiveConfig(cfg config.Config) {
	if rt == nil || rt.Manager == nil {
		return
	}
	rt.Config = cfg
	rt.Manager.mu.Lock()
	rt.Manager.activeManagement = cfg
	if rt.Manager.overrides.authStates != nil {
		authStates := cfg.AuthStates
		rt.Manager.overrides.authStates = &authStates
	}
	if rt.Manager.overrides.proxy != nil {
		proxy := cfg.Proxy
		rt.Manager.overrides.proxy = &proxy
	}
	cur := rt.Manager.current
	if cur != nil {
		cur.config = cfg
		if ra, ok := cur.admin.(*runtimeAdmin); ok && ra != nil {
			ra.config = cfg
		}
	}
	rt.Manager.mu.Unlock()
	if cur == nil || cur.service == nil {
		return
	}
	cur.service.ApplyCapacity(
		cfg.WarmWorkerLimit,
		cfg.MaxActiveWorkers,
		cfg.WarmStartupConcurrency,
		cfg.PerAccountConcurrency,
	)
	if ts, ok := cur.service.(*trackedService); ok && ts != nil {
		ts.SetFirstEventHedge(cfg.FirstEventHedge, cfg.FirstEventHedgeModels)
		ts.buildNativeNonstream = cfg.BuildNativeNonstream
		ts.maxAccountSwitches = cfg.MaxAccountSwitches
		ts.discoverHiddenModels = cfg.DiscoverHiddenModels
		ts.modelMapping, _ = config.ParseModelMapping(cfg.ModelMapping)
		if pooled, ok := ts.service.(*aistudio.PooledService); ok && pooled != nil {
			pooled.BuildNativeNonstream = cfg.BuildNativeNonstream
			pooled.YouTubeLinks = cfg.YouTubeLinks
			pooled.DiscoverHiddenModels = cfg.DiscoverHiddenModels
			pooled.StripThoughtSignatures = cfg.StripThoughtSignatures
			pooled.ContinuationRewrite = cfg.ContinuationRewrite
			pooled.ContinuationPrompt = cfg.ContinuationPrompt
		}
		if ts.pool != nil {
			ts.pool.SetRoutingStrategy(cfg.RoutingStrategy)
			ts.pool.SetAccountOrder(accountOrder(cfg.AccountOrder))
			ts.pool.SetUpstreamChannels(upstreamChannels(cfg.UpstreamChannels))
			ts.pool.SetModelChannels(modelChannels(cfg.ModelChannels))
			ts.pool.SetModelAccounts(modelAccounts(cfg.ModelAccounts))
			ts.pool.SetAssistantEnabled(cfg.EnableAssistantModels)
		}
	}
}

// EnsureRunning starts the generation service if it is currently STOPPED and at least one account exists.
func (rt *PluginRuntime) EnsureRunning(ctx context.Context) error {
	rt.Manager.mu.RLock()
	state := rt.Manager.current.service.State()
	admin := rt.Manager.current.admin
	rt.Manager.mu.RUnlock()
	if state == "RUNNING" || state == "LAUNCHING" {
		return nil
	}
	if pool := rt.Pool(); pool != nil {
		if _, eligible := pool.EnabledAccounts(); eligible == 0 {
			return aistudio.ErrNoEligibleAccount
		}
	}
	_, err := admin.StartService(ctx)
	return err
}

// Pool returns the current AccountPool.
func (rt *PluginRuntime) Pool() *aistudio.AccountPool {
	rt.Manager.mu.RLock()
	defer rt.Manager.mu.RUnlock()
	if ts, ok := rt.Manager.current.service.(*trackedService); ok {
		return ts.pool
	}
	return nil
}

// Store returns the current AccountStore.
func (rt *PluginRuntime) Store() *aistudio.AccountStore {
	rt.Manager.mu.RLock()
	defer rt.Manager.mu.RUnlock()
	if ra, ok := rt.Manager.current.admin.(*runtimeAdmin); ok {
		return ra.store
	}
	return nil
}

// RefreshAccountModels refreshes or loads the live model catalog for a single account.
func (rt *PluginRuntime) RefreshAccountModels(ctx context.Context, accountID string) ([]aistudio.Model, error) {
	rt.Manager.mu.RLock()
	ts, _ := rt.Manager.current.service.(*trackedService)
	rt.Manager.mu.RUnlock()
	if ts == nil {
		return nil, aistudio.ErrNoEligibleAccount
	}
	return ts.syncAccountModels(ctx, accountID)
}

// CachedModels returns the currently cached models across accounts.
func (rt *PluginRuntime) CachedModels() []aistudio.Model {
	rt.Manager.mu.RLock()
	ts, _ := rt.Manager.current.service.(*trackedService)
	rt.Manager.mu.RUnlock()
	if ts == nil {
		return nil
	}
	return ts.catalog.CachedModels()
}

// RecentLogs returns recent runtime logs.
func (rt *PluginRuntime) RecentLogs() []api.AdminLog {
	if rt == nil || rt.Manager == nil || rt.Manager.requests == nil {
		return nil
	}
	rt.Manager.requests.mu.Lock()
	defer rt.Manager.requests.mu.Unlock()
	return append([]api.AdminLog(nil), rt.Manager.requests.logs...)
}
