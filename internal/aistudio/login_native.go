package aistudio

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/proxydial"
	"github.com/gorilla/websocket"
)

const (
	chromeIncognitoStartURL = "https://aistudio.google.com/prompts/new_chat"
	aiStudioBasePrefix      = "https://aistudio.google.com/"
	aiStudioOriginURL       = "https://aistudio.google.com"
)

var wizEmailPattern = regexp.MustCompile(`"oPEP7c":("(?:[^"\\]|\\.)*")`)

const pageInspectExpression = `(() => {
  const visible = el => {
    if (!el) return false;
    try {
      if (typeof el.checkVisibility === 'function') {
        return el.checkVisibility({visibilityProperty: true});
      }
    } catch {}
    const rect = el.getBoundingClientRect();
    return rect.width > 0 && rect.height > 0;
  };
  const buttonEnabled = btn => btn && !btn.matches(':disabled') && !btn.closest('[aria-disabled="true"]');
  const dismissSelectors = [
    'ms-g1-welcome-dialog button[aria-label="Close dialog"]',
    'button[aria-label="Close guided tour"]',
    '#glue-cookie-notification-bar-1 .glue-cookie-notification-bar__reject'
  ];
  for (const sel of dismissSelectors) {
    try {
      const btn = [...document.querySelectorAll(sel)].find(visible);
      if (btn && buttonEnabled(btn)) btn.click();
    } catch {}
  }

  const ready = document.readyState === 'complete' || document.readyState === 'interactive';
  let email = '';
  try {
    const wiz = window.WIZ_global_data && window.WIZ_global_data.oPEP7c;
    if (typeof wiz === 'string' && wiz.includes('@')) email = wiz.trim();
  } catch {}
  if (!email) {
    const values = [document.body?.innerText || ''];
    for (const el of document.querySelectorAll('[aria-label],[data-email]')) {
      values.push(el.getAttribute('aria-label') || '');
      values.push(el.getAttribute('data-email') || '');
    }
    email = values.join('\n').match(/[A-Z0-9._%+-]+@[A-Z0-9.-]+\.[A-Z]{2,}/i)?.[0] || '';
  }
  let ls = [];
  try {
    ls = Object.keys(localStorage).sort().map(name => ({name, value: localStorage.getItem(name) || ''}));
  } catch {}

  const promptBox = [...document.querySelectorAll('ms-prompt-box textarea')].find(visible) || null;
  const blockingDialogs = [...document.querySelectorAll('mat-dialog-container, ms-tos-dialog, ms-onboarding')].filter(visible);
  const hasPrompt = Boolean(promptBox) && blockingDialogs.length === 0;
  const hasDialog = blockingDialogs.length > 0;

  let accessTokenStatus = Number(window.__aistudio_access_token_status || 0);
  if (!accessTokenStatus) {
    try {
      const entries = performance.getEntriesByType('resource').filter(e => e.name && e.name.endsWith('/GenerateAccessToken'));
      if (entries.length > 0) {
        const last = entries[entries.length - 1];
        if (typeof last.responseStatus === 'number' && last.responseStatus > 0) {
          accessTokenStatus = last.responseStatus;
        }
      }
    } catch {}
  }

  return JSON.stringify({
    ready,
    email: (email || '').toLowerCase().trim(),
    hasPrompt,
    hasDialog,
    accessTokenStatus,
    ls,
    href: location.href || ''
  });
})()`

// NativeLoginDriver 通过启动本机 Chrome/Edge/Brave 的独立无痕窗口（--incognito + 临时 --user-data-dir）完成 AI Studio 登录并自动拉取凭证
type NativeLoginDriver struct {
	chromePath string
	timeout    time.Duration
}

var _ IsolatedLoginDriver = (*NativeLoginDriver)(nil)

func NewNativeLoginDriver(browserPath string, timeout time.Duration) (*NativeLoginDriver, error) {
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	return &NativeLoginDriver{chromePath: strings.TrimSpace(browserPath), timeout: timeout}, nil
}

// FindLocalChromeBinary 查找本机已安装的 Chrome / Chromium / Edge / Brave 可执行文件
func FindLocalChromeBinary() (string, error) {
	for _, envKey := range []string{"AISTUDIO2API_CHROME_PATH", "CHROME_PATH", "BROWSER_PATH"} {
		if v := strings.TrimSpace(os.Getenv(envKey)); v != "" {
			if info, err := os.Stat(v); err == nil && !info.IsDir() {
				return v, nil
			}
		}
	}

	var candidates []string
	home, _ := os.UserHomeDir()

	switch runtime.GOOS {
	case "darwin":
		appRelPaths := []string{
			"Google Chrome.app/Contents/MacOS/Google Chrome",
			"Google Chrome Canary.app/Contents/MacOS/Google Chrome Canary",
			"Chromium.app/Contents/MacOS/Chromium",
			"Microsoft Edge.app/Contents/MacOS/Microsoft Edge",
			"Brave Browser.app/Contents/MacOS/Brave Browser",
		}
		for _, rel := range appRelPaths {
			candidates = append(candidates, filepath.Join("/Applications", rel))
			if home != "" {
				candidates = append(candidates, filepath.Join(home, "Applications", rel))
			}
		}
	case "windows":
		roots := []string{
			os.Getenv("ProgramFiles"),
			os.Getenv("ProgramFiles(x86)"),
			os.Getenv("LocalAppData"),
		}
		relPaths := []string{
			`Google\Chrome\Application\chrome.exe`,
			`Chromium\Application\chrome.exe`,
			`Microsoft\Edge\Application\msedge.exe`,
			`BraveSoftware\Brave-Browser\Application\brave.exe`,
		}
		for _, root := range roots {
			if strings.TrimSpace(root) == "" {
				continue
			}
			for _, rel := range relPaths {
				candidates = append(candidates, filepath.Join(root, rel))
			}
		}
	default:
		candidates = append(candidates,
			"/usr/bin/google-chrome",
			"/usr/bin/google-chrome-stable",
			"/usr/bin/chromium",
			"/usr/bin/chromium-browser",
			"/snap/bin/chromium",
			"/usr/bin/microsoft-edge",
			"/usr/bin/brave-browser",
		)
	}

	for _, c := range candidates {
		if info, err := os.Stat(c); err == nil && !info.IsDir() {
			return c, nil
		}
	}

	for _, binName := range []string{
		"google-chrome",
		"google-chrome-stable",
		"chromium",
		"chromium-browser",
		"chrome",
		"msedge",
		"microsoft-edge",
		"brave-browser",
	} {
		if path, err := exec.LookPath(binName); err == nil && path != "" {
			return path, nil
		}
	}

	return "", errors.New("未找到本机 Chrome / Edge / Brave 浏览器，请安装 Chrome 或设置环境变量 AISTUDIO2API_CHROME_PATH")
}

// Login 启动本机 Chrome 无痕窗口引导用户登录 Google AI Studio，并在登录完成后自动拉取 Cookie 与 StorageState
func (driver *NativeLoginDriver) Login(ctx context.Context, request IsolatedLoginRequest) (IsolatedLoginResult, error) {
	if runtime.GOOS == "linux" && os.Getenv("DISPLAY") == "" && os.Getenv("WAYLAND_DISPLAY") == "" {
		return IsolatedLoginResult{}, errors.New("当前服务器无图形桌面环境（缺少 DISPLAY / WAYLAND_DISPLAY），无法弹出本机 Chrome 窗口，请粘贴 Cookie 导入")
	}

	chromeBin := strings.TrimSpace(driver.chromePath)
	if chromeBin != "" {
		if info, statErr := os.Stat(chromeBin); statErr != nil || info.IsDir() {
			chromeBin = ""
		}
	}
	if chromeBin == "" {
		var err error
		chromeBin, err = FindLocalChromeBinary()
		if err != nil {
			return IsolatedLoginResult{}, err
		}
	}

	timeout := driver.timeout
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	loginCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	userDataDir, err := os.MkdirTemp("", "aistudio2api-chrome-incognito-*")
	if err != nil {
		return IsolatedLoginResult{}, fmt.Errorf("创建临时 Chrome 无痕目录: %w", err)
	}
	defer func() {
		for range 6 {
			if err := os.RemoveAll(userDataDir); err == nil {
				return
			}
			time.Sleep(150 * time.Millisecond)
		}
	}()

	args := []string{
		"--user-data-dir=" + userDataDir,
		"--remote-debugging-port=0",
		"--disable-blink-features=AutomationControlled",
		"--disable-features=EnableBoundSessionCredentials,DeviceBoundSessions,PersistDeviceBoundSessions,EnableChromeRefreshTokenBinding",
		"--exclude-switches=enable-automation",
		"--no-first-run",
		"--no-default-browser-check",
		"--new-window",
	}
	if proxy := strings.TrimSpace(request.Proxy); proxy != "" {
		parsedProxy, parseErr := url.Parse(proxy)
		if parseErr != nil {
			return IsolatedLoginResult{}, fmt.Errorf("解析登录代理失败: %w", parseErr)
		}
		if parsedProxy.User != nil {
			relay, relayErr := proxydial.StartRelay(parsedProxy)
			if relayErr != nil {
				return IsolatedLoginResult{}, fmt.Errorf("启动登录代理中转失败: %w", relayErr)
			}
			defer relay.Close()
			proxy = "socks5://" + relay.Addr().String()
		}
		args = append(args, "--proxy-server="+proxy)
	}
	if locale := strings.TrimSpace(request.Locale); locale != "" {
		args = append(args, "--lang="+locale)
	}
	args = append(args, chromeIncognitoStartURL)

	cmd := exec.Command(chromeBin, args...)
	if err := cmd.Start(); err != nil {
		return IsolatedLoginResult{}, fmt.Errorf("启动本机 Chrome 无痕窗口失败: %w", err)
	}

	var exited atomic.Bool
	var exitMu sync.Mutex
	var exitErr error
	waitDone := make(chan struct{})
	go func() {
		err := cmd.Wait()
		exitMu.Lock()
		exitErr = err
		exitMu.Unlock()
		exited.Store(true)
		close(waitDone)
	}()
	readExitErr := func() error {
		exitMu.Lock()
		defer exitMu.Unlock()
		return exitErr
	}

	var debugPort atomic.Int64
	closeBrowser := func(pageWS string) error {
		targetWS := strings.TrimSpace(pageWS)
		if targetWS == "" {
			if port := int(debugPort.Load()); port > 0 {
				targetWS = discoverBrowserWSURL(port)
			}
		}
		if targetWS != "" {
			closeCtx, closeCancel := context.WithTimeout(context.Background(), 2*time.Second)
			_ = cdpCloseBrowser(closeCtx, targetWS)
			closeCancel()
		}
		if !exited.Load() && cmd.Process != nil {
			if runtime.GOOS == "windows" {
				_ = exec.Command("taskkill", "/F", "/T", "/PID", strconv.Itoa(cmd.Process.Pid)).Run()
			}
			_ = cmd.Process.Kill()
		}
		select {
		case <-waitDone:
		case <-time.After(3 * time.Second):
		}
		return nil
	}
	defer func() {
		_ = closeBrowser("")
	}()

	port, detached, err := waitForDevToolsActivePortWithGrace(loginCtx, userDataDir, &exited, waitDone, readExitErr, 8*time.Second)
	if err != nil {
		return IsolatedLoginResult{}, err
	}
	debugPort.Store(int64(port))

	// 阶段 1：轮询 /json/list，等待用户登录 Google 并进入 AI Studio 且输入框就绪（新号需完成条款弹窗）
	var activePageWS string
	var verifiedEmail string
	var capturedState StorageState
	var promptReadySince time.Time
	var driveError string

	httpClient := &http.Client{Timeout: 3 * time.Second}
	consecutiveCDPErrors := 0
	for {
		if err := loginCtx.Err(); err != nil {
			return IsolatedLoginResult{}, fmt.Errorf("等待 Chrome 窗口登录超时或已取消: %w", err)
		}

		targets, err := listCDPTargets(loginCtx, httpClient, port)
		if err != nil {
			consecutiveCDPErrors++
			if !detached && exited.Load() {
				return IsolatedLoginResult{}, errors.New("Chrome 登录窗口已关闭，登录已取消")
			}
			if detached && consecutiveCDPErrors >= 3 {
				return IsolatedLoginResult{}, errors.New("Chrome 登录窗口已关闭，登录已取消")
			}
			select {
			case <-loginCtx.Done():
				return IsolatedLoginResult{}, loginCtx.Err()
			case <-time.After(400 * time.Millisecond):
				continue
			}
		}
		consecutiveCDPErrors = 0
		if !detached && exited.Load() {
			// 原进程已退出但同目录的 CDP 服务仍存活（例如 Windows Chrome 降权重启子进程），切换为 CDP 存活检测
			detached = true
		}

		var aiStudioTarget *cdpPageTarget
		for i := range targets {
			t := &targets[i]
			if t.Type == "page" && t.WebSocketDebuggerURL != "" {
				ensureStealthOnTarget(loginCtx, t.WebSocketDebuggerURL)
				if strings.HasPrefix(t.URL, aiStudioBasePrefix) {
					aiStudioTarget = t
				}
			}
		}
		if aiStudioTarget == nil {
			promptReadySince = time.Time{}
			select {
			case <-loginCtx.Done():
				return IsolatedLoginResult{}, loginCtx.Err()
			case <-time.After(450 * time.Millisecond):
				continue
			}
		}
		activePageWS = aiStudioTarget.WebSocketDebuggerURL

		// 阶段 2：检查 AI Studio 提示词输入框是否已就绪（无条款/引导弹窗遮挡）并读取 GenerateAccessToken 状态
		state, info, err := inspectAIStudioPage(loginCtx, activePageWS)
		if err == nil && len(state.Cookies) > 0 && info.HasPrompt {
			if promptReadySince.IsZero() {
				promptReadySince = time.Now()
			}
			if _, signErr := NewSigner().Authorization(state); signErr == nil {
				email := strings.ToLower(strings.TrimSpace(info.Email))
				if email == "" && time.Since(promptReadySince) > 1500*time.Millisecond {
					email, _ = resolveEmailViaHTTP(loginCtx, &state, request.Proxy)
				}
				if email != "" && strings.Contains(email, "@") {
					// 等待页面发出 GenerateAccessToken 请求以判断 Google Drive 是否已授权
					if info.AccessTokenStatus == 0 && time.Since(promptReadySince) < 3500*time.Millisecond {
						select {
						case <-loginCtx.Done():
							return IsolatedLoginResult{}, loginCtx.Err()
						case <-time.After(400 * time.Millisecond):
							continue
						}
					}
					verifiedEmail = email
					if info.AccessTokenStatus == http.StatusUnauthorized {
						// 阶段 3：与原版一致，新账号未授权 Google Drive 时自动跳转 /library 点击 Allow Drive access 并完成 OAuth 授权弹窗
						if authErr := authorizeDriveOverCDP(loginCtx, httpClient, port, activePageWS, verifiedEmail, &exited, detached); authErr != nil {
							driveError = authErr.Error()
						}
						if refreshedState, _, refreshErr := inspectAIStudioPage(loginCtx, activePageWS); refreshErr == nil && len(refreshedState.Cookies) > 0 {
							state = refreshedState
						}
					}
					capturedState = state
					break
				}
			}
		} else {
			promptReadySince = time.Time{}
		}

		select {
		case <-loginCtx.Done():
			return IsolatedLoginResult{}, loginCtx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}

	closeErr := closeBrowser(activePageWS)
	capturedState.SetAuthExtension(AuthExtension{
		Source: AuthSource{
			Browser: "chrome-incognito",
			Email:   verifiedEmail,
		},
	})
	var closeErrText string
	if closeErr != nil {
		closeErrText = closeErr.Error()
	}
	return IsolatedLoginResult{
		StorageState: capturedState,
		Email:        verifiedEmail,
		VerifiedAt:   time.Now().UTC(),
		DriveError:   driveError,
		CloseError:   closeErrText,
	}, nil
}

type cdpPageTarget struct {
	ID                   string `json:"id"`
	Type                 string `json:"type"`
	Title                string `json:"title"`
	URL                  string `json:"url"`
	WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
}

func readDevToolsActivePort(portFile string) (int, bool) {
	data, err := os.ReadFile(portFile)
	if err != nil {
		return 0, false
	}
	line, rest, hasNewline := strings.Cut(string(data), "\n")
	if !hasNewline || strings.TrimSpace(rest) == "" {
		return 0, false
	}
	port, convErr := strconv.Atoi(strings.TrimSpace(line))
	if convErr != nil || port <= 0 || port > 65535 {
		return 0, false
	}
	return port, true
}

func waitForDevToolsActivePort(ctx context.Context, userDataDir string, exited *atomic.Bool) (int, error) {
	port, _, err := waitForDevToolsActivePortWithGrace(ctx, userDataDir, exited, nil, nil, 8*time.Second)
	return port, err
}

func waitForDevToolsActivePortWithGrace(
	ctx context.Context,
	userDataDir string,
	exited *atomic.Bool,
	waitDone <-chan struct{},
	exitErrFn func() error,
	exitGrace time.Duration,
) (int, bool, error) {
	portFile := filepath.Join(userDataDir, "DevToolsActivePort")
	if exitGrace <= 0 {
		exitGrace = 8 * time.Second
	}
	var exitedAt time.Time
	for {
		if port, ok := readDevToolsActivePort(portFile); ok {
			return port, exited != nil && exited.Load(), nil
		}
		if err := ctx.Err(); err != nil {
			return 0, false, fmt.Errorf("等待 Chrome 调试端口超时: %w", err)
		}
		if exited != nil && exited.Load() {
			if exitedAt.IsZero() {
				exitedAt = time.Now()
			} else if time.Since(exitedAt) >= exitGrace {
				var exitDetail error
				if exitErrFn != nil {
					exitDetail = exitErrFn()
				}
				if exitDetail != nil {
					return 0, false, fmt.Errorf("Chrome 无痕窗口启动后立即退出（%v）（请确认本机 Chrome 可正常打开）", exitDetail)
				}
				return 0, false, errors.New("Chrome 无痕窗口启动后立即退出（请确认本机 Chrome 可正常打开）")
			}
		}
		pollDelay := 120 * time.Millisecond
		if waitDone != nil && exitedAt.IsZero() {
			select {
			case <-ctx.Done():
				return 0, false, ctx.Err()
			case <-waitDone:
				exitedAt = time.Now()
			case <-time.After(pollDelay):
			}
			continue
		}
		select {
		case <-ctx.Done():
			return 0, false, ctx.Err()
		case <-time.After(pollDelay):
		}
	}
}

func discoverBrowserWSURL(port int) string {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	client := &http.Client{Timeout: time.Second}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/json/version", port), nil)
	if err == nil {
		if resp, doErr := client.Do(req); doErr == nil {
			defer resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				var version struct {
					WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
				}
				if json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&version) == nil && strings.TrimSpace(version.WebSocketDebuggerURL) != "" {
					return strings.TrimSpace(version.WebSocketDebuggerURL)
				}
			}
		}
	}
	if targets, err := listCDPTargets(ctx, client, port); err == nil && len(targets) > 0 {
		return strings.TrimSpace(targets[0].WebSocketDebuggerURL)
	}
	return ""
}
func listCDPTargets(ctx context.Context, client *http.Client, port int) ([]cdpPageTarget, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/json/list", port), nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return nil, err
	}
	var targets []cdpPageTarget
	if err := json.Unmarshal(body, &targets); err != nil {
		return nil, err
	}
	return targets, nil
}

type cdpPageInspection struct {
	Ready             bool          `json:"ready"`
	Email             string        `json:"email"`
	HasPrompt         bool          `json:"hasPrompt"`
	HasDialog         bool          `json:"hasDialog"`
	AccessTokenStatus int           `json:"accessTokenStatus"`
	LS                []StorageItem `json:"ls"`
	Href              string        `json:"href"`
}

func inspectAIStudioPage(ctx context.Context, pageWS string) (StorageState, cdpPageInspection, error) {
	callCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	dialer := websocket.Dialer{HandshakeTimeout: 3 * time.Second}
	conn, _, err := dialer.DialContext(callCtx, pageWS, nil)
	if err != nil {
		return StorageState{}, cdpPageInspection{}, err
	}
	defer conn.Close()
	stop := context.AfterFunc(callCtx, func() { _ = conn.Close() })
	defer stop()

	var rawCookies struct {
		Cookies []struct {
			Name         string  `json:"name"`
			Value        string  `json:"value"`
			Domain       string  `json:"domain"`
			Path         string  `json:"path"`
			Expires      float64 `json:"expires"`
			HTTPOnly     bool    `json:"httpOnly"`
			Secure       bool    `json:"secure"`
			SameSite     string  `json:"sameSite"`
			PartitionKey any     `json:"partitionKey"`
		} `json:"cookies"`
	}
	if err := cdpCall(callCtx, conn, 1, "Network.getAllCookies", map[string]any{}, &rawCookies); err != nil {
		return StorageState{}, cdpPageInspection{}, err
	}

	cookies := make([]StateCookie, 0, len(rawCookies.Cookies))
	for _, item := range rawCookies.Cookies {
		if item.Name == "" || item.Domain == "" {
			continue
		}
		path := item.Path
		if path == "" {
			path = "/"
		}
		expires := item.Expires
		if expires <= 0 {
			expires = -1
		}
		sameSite := strings.ToLower(strings.TrimSpace(item.SameSite))
		switch sameSite {
		case "none":
			sameSite = "None"
		case "lax":
			sameSite = "Lax"
		case "strict":
			sameSite = "Strict"
		default:
			sameSite = ""
		}
		partKey, _ := item.PartitionKey.(string)
		if partKey == "" {
			if pkMap, ok := item.PartitionKey.(map[string]any); ok {
				if hasCrossSite, _ := pkMap["hasCrossSiteAncestor"].(bool); hasCrossSite {
					continue
				}
				partKey, _ = pkMap["topLevelSite"].(string)
			}
		}
		cookies = append(cookies, StateCookie{
			Name:         item.Name,
			Value:        item.Value,
			Domain:       item.Domain,
			Path:         path,
			Expires:      expires,
			HTTPOnly:     item.HTTPOnly,
			Secure:       item.Secure,
			SameSite:     sameSite,
			PartitionKey: partKey,
		})
	}
	sort.SliceStable(cookies, func(i, j int) bool {
		if cookies[i].Domain != cookies[j].Domain {
			return cookies[i].Domain < cookies[j].Domain
		}
		if cookies[i].Name != cookies[j].Name {
			return cookies[i].Name < cookies[j].Name
		}
		return cookies[i].Path < cookies[j].Path
	})

	var evalResp struct {
		Result struct {
			Type  string `json:"type"`
			Value string `json:"value"`
		} `json:"result"`
	}
	var stealthOut map[string]any
	_ = cdpCall(callCtx, conn, 2, "Runtime.evaluate", map[string]any{
		"expression": stealthInitScript,
	}, &stealthOut)

	var info cdpPageInspection
	if err := cdpCall(callCtx, conn, 3, "Runtime.evaluate", map[string]any{
		"expression":    pageInspectExpression,
		"returnByValue": true,
	}, &evalResp); err == nil && evalResp.Result.Value != "" {
		_ = json.Unmarshal([]byte(evalResp.Result.Value), &info)
	}

	var origins []StorageOrigin
	if len(info.LS) > 0 {
		origins = []StorageOrigin{{
			Origin:       aiStudioOriginURL,
			LocalStorage: info.LS,
		}}
	}
	state := StorageState{
		Cookies: cookies,
		Origins: origins,
	}
	info.Email = strings.ToLower(strings.TrimSpace(info.Email))
	return state, info, nil
}

func resolveEmailViaHTTP(ctx context.Context, state *StorageState, proxy string) (string, error) {
	reqCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()

	client, err := NewProxyHTTPClient(proxy)
	if err != nil {
		return "", err
	}
	defer client.CloseIdleConnections()
	client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}

	cookieHeader, err := state.CookieHeader(aiStudioOriginURL+"/prompts/new_chat", time.Now())
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, aiStudioOriginURL+"/prompts/new_chat", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Cookie", cookieHeader)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return "", err
	}
	match := wizEmailPattern.FindSubmatch(body)
	if len(match) < 2 {
		return "", errors.New("email not found in WIZ_global_data")
	}
	var email string
	if err := json.Unmarshal(match[1], &email); err != nil {
		return "", err
	}
	return strings.ToLower(strings.TrimSpace(email)), nil
}

func cdpCloseBrowser(ctx context.Context, pageWS string) error {
	dialer := websocket.Dialer{HandshakeTimeout: 2 * time.Second}
	conn, _, err := dialer.DialContext(ctx, pageWS, nil)
	if err != nil {
		return err
	}
	defer conn.Close()
	var out map[string]any
	return cdpCall(ctx, conn, 99, "Browser.close", map[string]any{}, &out)
}

const stealthInitScript = `(() => {
  try {
    Object.defineProperty(navigator, 'webdriver', { get: () => undefined });
  } catch {}
  try {
    delete Object.getPrototypeOf(navigator).webdriver;
  } catch {}
  try {
    for (const k of Object.keys(window)) {
      if (k.startsWith('cdc_') || k.startsWith('$cdc_')) {
        try { delete window[k]; } catch {}
      }
    }
  } catch {}
  try {
    if (!window.__aistudio_xhr_hooked) {
      window.__aistudio_xhr_hooked = true;
      const origOpen = XMLHttpRequest.prototype.open;
      XMLHttpRequest.prototype.open = function(method, url, ...rest) {
        try {
          if (typeof url === 'string' && url.includes('/GenerateAccessToken')) {
            this.addEventListener('loadend', () => {
              if (this.status > 0) {
                window.__aistudio_access_token_status = this.status;
              }
            });
          }
        } catch {}
        return origOpen.call(this, method, url, ...rest);
      };
      const origFetch = window.fetch;
      if (typeof origFetch === 'function') {
        window.fetch = async function(input, init) {
          const resp = await origFetch.call(this, input, init);
          try {
            const url = typeof input === 'string' ? input : (input && input.url) || '';
            if (url.includes('/GenerateAccessToken') && resp && resp.status > 0) {
              window.__aistudio_access_token_status = resp.status;
            }
          } catch {}
          return resp;
        };
      }
    }
  } catch {}
})();`

func ensureStealthOnTarget(ctx context.Context, pageWS string) {
	if pageWS == "" {
		return
	}
	callCtx, cancel := context.WithTimeout(ctx, 1500*time.Millisecond)
	defer cancel()
	dialer := websocket.Dialer{HandshakeTimeout: 1 * time.Second}
	conn, _, err := dialer.DialContext(callCtx, pageWS, nil)
	if err != nil {
		return
	}
	defer conn.Close()
	var out map[string]any
	_ = cdpCall(callCtx, conn, 1, "Page.enable", map[string]any{}, &out)
	_ = cdpCall(callCtx, conn, 2, "Page.addScriptToEvaluateOnNewDocument", map[string]any{
		"source": stealthInitScript,
	}, &out)
	_ = cdpCall(callCtx, conn, 3, "Runtime.evaluate", map[string]any{
		"expression": stealthInitScript,
	}, &out)
}

const driveAccessButtonExpr = `(() => {
  const visible = el => {
    if (!el) return false;
    const box = el.getBoundingClientRect();
    if (box.width <= 0 || box.height <= 0) return false;
    const hit = document.elementFromPoint(box.left + box.width / 2, box.top + box.height / 2);
    return Boolean(hit && (el.contains(hit) || hit.contains(el)));
  };
  const candidates = [...document.querySelectorAll('button[aria-label="Allow Drive access"], button')];
  return candidates.find(btn => {
    const text = ((btn.getAttribute('aria-label') || '') + ' ' + (btn.innerText || '')).trim();
    return /Allow Drive access|Enable Drive|允许访问|云端硬盘/i.test(text) && visible(btn);
  }) || null;
})()`

const driveApproveButtonExpr = `(() => {
  const visible = el => {
    if (!el) return false;
    const box = el.getBoundingClientRect();
    return box.width > 0 && box.height > 0 && !el.matches(':disabled') && !el.closest('[aria-disabled="true"]');
  };
  const direct = document.querySelector('#submit_approve_access');
  if (visible(direct)) return direct;
  const candidates = [...document.querySelectorAll('div[data-continue-button] button, button[jsname], button, input[type="submit"]')];
  return candidates.find(btn => {
    if (!visible(btn)) return false;
    const text = ((btn.getAttribute('aria-label') || '') + ' ' + (btn.innerText || btn.value || '')).trim();
    return /^(Allow|Continue|允许|继续)$/i.test(text);
  }) || null;
})()`

func authorizeDriveOverCDP(
	ctx context.Context,
	httpClient *http.Client,
	debugPort int,
	mainPageWS string,
	email string,
	exited *atomic.Bool,
	detached bool,
) error {
	driveCtx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()

	initialTargets, err := listCDPTargets(driveCtx, httpClient, debugPort)
	if err != nil {
		return err
	}
	knownIDs := make(map[string]struct{}, len(initialTargets))
	for _, t := range initialTargets {
		knownIDs[t.ID] = struct{}{}
	}

	if err := cdpNavigate(driveCtx, mainPageWS, aiStudioOriginURL+"/library"); err != nil {
		return fmt.Errorf("导航至 /library 授权 Drive: %w", err)
	}

	// 等待 /library 页面出现 Allow Drive access 按钮并点击打开 OAuth 授权弹窗
	var popupTarget *cdpPageTarget
	for popupTarget == nil {
		if err := driveCtx.Err(); err != nil {
			return fmt.Errorf("等待 Drive 授权弹窗超时: %w", err)
		}
		if !detached && exited.Load() {
			if _, err := listCDPTargets(driveCtx, httpClient, debugPort); err != nil {
				return errors.New("浏览器窗口已关闭")
			}
			detached = true
		}
		_ = cdpClickElement(driveCtx, mainPageWS, driveAccessButtonExpr)

		waitPopupDeadline := time.Now().Add(4 * time.Second)
		for time.Now().Before(waitPopupDeadline) {
			targets, err := listCDPTargets(driveCtx, httpClient, debugPort)
			if err == nil {
				for i := range targets {
					t := &targets[i]
					if t.Type == "page" && t.WebSocketDebuggerURL != "" {
						ensureStealthOnTarget(driveCtx, t.WebSocketDebuggerURL)
						if _, known := knownIDs[t.ID]; !known {
							popupTarget = t
							break
						}
					}
				}
			}
			if popupTarget != nil {
				break
			}
			select {
			case <-driveCtx.Done():
				return driveCtx.Err()
			case <-time.After(250 * time.Millisecond):
			}
		}
	}

	// 在 OAuth 授权弹窗中自动选择当前登录邮箱并点击同意（同时允许用户手动点击），直到弹窗关闭
	emailJSON, _ := json.Marshal(strings.ToLower(strings.TrimSpace(email)))
	accountSelectExpr := fmt.Sprintf(`((email) => {
  const items = [...document.querySelectorAll('[data-email]')];
  return items.find(item => (item.getAttribute('data-email') || '').toLowerCase() === email) || (items.length === 1 ? items[0] : null);
})(%s)`, string(emailJSON))

	consecutiveDriveCDPErrors := 0
	for {
		if err := driveCtx.Err(); err != nil {
			return fmt.Errorf("等待 Drive OAuth 弹窗完成超时: %w", err)
		}
		targets, err := listCDPTargets(driveCtx, httpClient, debugPort)
		if err != nil {
			consecutiveDriveCDPErrors++
			if (!detached && exited.Load()) || (detached && consecutiveDriveCDPErrors >= 3) {
				return errors.New("浏览器窗口已关闭")
			}
			select {
			case <-driveCtx.Done():
				return driveCtx.Err()
			case <-time.After(450 * time.Millisecond):
				continue
			}
		}
		consecutiveDriveCDPErrors = 0
		if !detached && exited.Load() {
			detached = true
		}
		var currentPopup *cdpPageTarget
		for i := range targets {
			if targets[i].ID == popupTarget.ID {
				currentPopup = &targets[i]
				break
			}
		}
		if currentPopup == nil {
			// 弹窗已关闭，Drive OAuth 授权完成
			break
		}
		if currentPopup.WebSocketDebuggerURL != "" {
			ensureStealthOnTarget(driveCtx, currentPopup.WebSocketDebuggerURL)
			step, _ := cdpEvalString(driveCtx, currentPopup.WebSocketDebuggerURL, fmt.Sprintf(
				`(() => (%s) ? 'approve' : (%s) ? 'account' : '')()`,
				driveApproveButtonExpr,
				accountSelectExpr,
			))
			switch step {
			case "approve":
				_ = cdpClickElement(driveCtx, currentPopup.WebSocketDebuggerURL, driveApproveButtonExpr)
			case "account":
				_ = cdpClickElement(driveCtx, currentPopup.WebSocketDebuggerURL, accountSelectExpr)
			}
		}
		select {
		case <-driveCtx.Done():
			return driveCtx.Err()
		case <-time.After(450 * time.Millisecond):
		}
	}

	// 切回 /prompts/new_chat 并确认 GenerateAccessToken 返回 200
	_, _ = cdpEvalString(driveCtx, mainPageWS, `(() => { window.__aistudio_access_token_status = 0; try { performance.clearResourceTimings(); } catch {} return 'ok'; })()`)
	if err := cdpNavigate(driveCtx, mainPageWS, aiStudioOriginURL+"/prompts/new_chat"); err != nil {
		return err
	}
	verifyDeadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(verifyDeadline) {
		if _, info, err := inspectAIStudioPage(driveCtx, mainPageWS); err == nil {
			if info.AccessTokenStatus == http.StatusOK {
				return nil
			}
		}
		select {
		case <-driveCtx.Done():
			return driveCtx.Err()
		case <-time.After(350 * time.Millisecond):
		}
	}
	return nil
}

func cdpNavigate(ctx context.Context, pageWS string, targetURL string) error {
	callCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	dialer := websocket.Dialer{HandshakeTimeout: 2 * time.Second}
	conn, _, err := dialer.DialContext(callCtx, pageWS, nil)
	if err != nil {
		return err
	}
	defer conn.Close()
	var out map[string]any
	return cdpCall(callCtx, conn, 10, "Page.navigate", map[string]any{"url": targetURL}, &out)
}

func cdpEvalString(ctx context.Context, pageWS string, expression string) (string, error) {
	callCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	dialer := websocket.Dialer{HandshakeTimeout: 2 * time.Second}
	conn, _, err := dialer.DialContext(callCtx, pageWS, nil)
	if err != nil {
		return "", err
	}
	defer conn.Close()
	var evalResp struct {
		Result struct {
			Value string `json:"value"`
		} `json:"result"`
	}
	if err := cdpCall(callCtx, conn, 11, "Runtime.evaluate", map[string]any{
		"expression":    expression,
		"returnByValue": true,
	}, &evalResp); err != nil {
		return "", err
	}
	return evalResp.Result.Value, nil
}

func cdpClickElement(ctx context.Context, pageWS string, elementExpr string) error {
	coordExpr := fmt.Sprintf(`(() => {
  const el = %s;
  if (!el) return '';
  try { el.scrollIntoView({block: 'center', inline: 'center', behavior: 'instant'}); } catch {}
  const box = el.getBoundingClientRect();
  if (box.width <= 0 || box.height <= 0) return '';
  return JSON.stringify([box.left + box.width / 2, box.top + box.height / 2]);
})()`, elementExpr)
	encoded, err := cdpEvalString(ctx, pageWS, coordExpr)
	if err != nil || encoded == "" {
		return errors.New("element not visible")
	}
	var pt [2]float64
	if err := json.Unmarshal([]byte(encoded), &pt); err != nil {
		return err
	}
	x, y := math.Round(pt[0]), math.Round(pt[1])

	callCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	dialer := websocket.Dialer{HandshakeTimeout: 2 * time.Second}
	conn, _, err := dialer.DialContext(callCtx, pageWS, nil)
	if err != nil {
		return err
	}
	defer conn.Close()
	var out map[string]any
	_ = cdpCall(callCtx, conn, 20, "Input.dispatchMouseEvent", map[string]any{
		"type": "mouseMoved",
		"x":    x,
		"y":    y,
	}, &out)
	_ = cdpCall(callCtx, conn, 21, "Input.dispatchMouseEvent", map[string]any{
		"type":       "mousePressed",
		"x":          x,
		"y":          y,
		"button":     "left",
		"clickCount": 1,
	}, &out)
	return cdpCall(callCtx, conn, 22, "Input.dispatchMouseEvent", map[string]any{
		"type":       "mouseReleased",
		"x":          x,
		"y":          y,
		"button":     "left",
		"clickCount": 1,
	}, &out)
}

func cdpCall(ctx context.Context, conn *websocket.Conn, id int, method string, params any, result any) error {
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetWriteDeadline(deadline)
		_ = conn.SetReadDeadline(deadline)
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	msg := map[string]any{
		"id":     id,
		"method": method,
	}
	if params != nil {
		msg["params"] = params
	}
	if err := conn.WriteJSON(msg); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return err
	}
	for {
		var resp struct {
			ID     int             `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := conn.ReadJSON(&resp); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
		if resp.ID != id {
			continue
		}
		if resp.Error != nil {
			return errors.New(resp.Error.Message)
		}
		if result != nil && len(resp.Result) > 0 {
			return json.Unmarshal(resp.Result, result)
		}
		return nil
	}
}
