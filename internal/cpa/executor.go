package cpa

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/app"
	"github.com/Mag1cFall/AIStudio2API/internal/pluginabi"
	"github.com/Mag1cFall/AIStudio2API/internal/pluginapi"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

type rpcExecutorRequest struct {
	pluginapi.ExecutorRequest
	StreamID       string `json:"stream_id,omitempty"`
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

type rpcExecutorHTTPRequest struct {
	pluginapi.ExecutorHTTPRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

type rpcExecutorStreamResponse struct {
	Headers http.Header                     `json:"headers,omitempty"`
	Chunks  []pluginapi.ExecutorStreamChunk `json:"chunks,omitempty"`
}

func (p *Plugin) handleExecutorIdentifier() (any, error) {
	return rpcIdentifierResponse{
		Identifier:          ProviderID,
		Identifiers:         []string{ProviderID},
		CredentialProviders: []string{"aistudio2api", "aistudio", "google-aistudio"},
	}, nil
}

func (p *Plugin) prepareExecutionContext(
	baseCtx context.Context,
	targetModel string,
	storageJSON []byte,
	authMetadata map[string]any,
	authAttributes map[string]string,
) (context.Context, *app.PluginRuntime, string, error) {
	rt, err := p.Runtime()
	if err != nil {
		return nil, nil, "", err
	}

	accountID := extractAccountIDFromAuth(nil, authMetadata, authAttributes)
	needUpsert := true
	if accountID != "" && rt.Pool() != nil {
		if existing, ok := rt.Pool().AccountByID(accountID); ok && existing != nil && existing.State != aistudio.AccountAuthRequired {
			needUpsert = false
		}
	}
	if needUpsert && len(storageJSON) > 0 {
		var payload AuthFilePayload
		_ = json.Unmarshal(storageJSON, &payload)
		if accountID == "" {
			accountID = extractAccountIDFromAuth(storageJSON, authMetadata, authAttributes)
		}
		if state, ok := extractStorageStateFromPayload(storageJSON, payload); ok {
			if acc, uErr := p.upsertAccountInRuntime(baseCtx, rt, accountID, payload, state, false); uErr == nil && acc.ID != "" {
				accountID = strings.ToLower(strings.TrimSpace(acc.ID))
			}
		}
	}

	if err := rt.EnsureRunning(baseCtx); err != nil {
		return nil, nil, accountID, pluginabi.NewError("service_unavailable", "AIStudio2API runtime failed to start: "+err.Error(), http.StatusServiceUnavailable)
	}

	reqCtx := baseCtx
	if accountID != "" && p.ShouldPinExecutorAccount(baseCtx, rt, accountID, targetModel) {
		if acc, ok := rt.Pool().AccountByID(accountID); ok && acc != nil {
			reqCtx = app.ContextWithPinnedAccountID(reqCtx, acc.ID)
		}
	}
	return reqCtx, rt, accountID, nil
}

// ShouldPinExecutorAccount reports whether an incoming executor request should pin
// execution to accountID, unpinning when accountID is degraded or cooling down on
// targetModel while other pool accounts remain enabled.
func (p *Plugin) ShouldPinExecutorAccount(
	ctx context.Context,
	rt *app.PluginRuntime,
	accountID string,
	targetModel string,
) bool {
	if !boolOrDefault(p.Config().PinExecutorAccount, true) || rt == nil || rt.Pool() == nil {
		return false
	}
	acc, ok := rt.Pool().AccountByID(strings.ToLower(strings.TrimSpace(accountID)))
	if !ok || acc == nil || !acc.Config.Enabled {
		return false
	}
	switch acc.State {
	case aistudio.AccountAuthRequired, aistudio.AccountRestricted, aistudio.AccountUnavailable:
		p.syncDegradedAccountIfChanged("", rt, acc.ID)
		return false
	}
	if len(rt.Pool().EnabledAccounts()) <= 1 {
		return true
	}
	cleanModel := StripModelPrefix(targetModel, p.Config().ModelPrefix)
	cleanModel = strings.TrimPrefix(cleanModel, "models/")
	if baseModel, _, suffixErr := aistudio.ApplyModelSuffix(cleanModel, aistudio.GenerationConfig{}); suffixErr == nil && baseModel != "" {
		cleanModel = baseModel
	}
	cleanModel = rt.Pool().CanonicalModelID(cleanModel)
	if cleanModel == "" {
		return !rt.Pool().AccountCoolingDown(acc.ID, "")
	}
	sel := aistudio.AccountSelection{
		ModelID:           cleanModel,
		Method:            inferUpstreamMethodForModel(cleanModel),
		AllowedAccountIDs: []string{acc.ID},
	}
	groups, err := rt.Pool().ClassifyCandidates(ctx, sel, nil)
	if err != nil {
		return !rt.Pool().AccountCoolingDown(acc.ID, cleanModel)
	}
	total := len(groups.WarmReady) + len(groups.StandbyReady) + len(groups.WarmAvailable) + len(groups.StandbyBusy) + len(groups.WarmBusy)
	if total == 0 && sel.Method != "" && groups.EarliestCooldown.Until.IsZero() {
		sel.Method = ""
		if fallback, fErr := rt.Pool().ClassifyCandidates(ctx, sel, nil); fErr == nil {
			total = len(fallback.WarmReady) + len(fallback.StandbyReady) + len(fallback.WarmAvailable) + len(fallback.StandbyBusy) + len(fallback.WarmBusy)
		}
	}
	return total > 0
}

func resolveExecutorTargetModel(model string, payload []byte) string {
	if trimmed := strings.TrimSpace(model); trimmed != "" {
		return trimmed
	}
	if len(payload) > 0 {
		return strings.TrimSpace(gjson.GetBytes(payload, "model").String())
	}
	return ""
}

func patchJSONModelAndStream(
	payload []byte,
	model string,
	setModelIfMissing bool,
	setStream bool,
	stream bool,
) []byte {
	if len(payload) == 0 {
		return payload
	}
	body := payload
	if model != "" {
		cur := gjson.GetBytes(body, "model")
		if (cur.Exists() || setModelIfMissing) && cur.String() != model {
			if updated, err := sjson.SetBytes(body, "model", model); err == nil {
				body = updated
			}
		}
	}
	if setStream {
		cur := gjson.GetBytes(body, "stream")
		alreadyMatches := (stream && cur.Type == gjson.True) || (!stream && cur.Type == gjson.False)
		if !alreadyMatches {
			if updated, err := sjson.SetBytes(body, "stream", stream); err == nil {
				body = updated
			}
		}
	}
	return body
}

func (p *Plugin) buildInternalHTTPRequest(
	ctx context.Context,
	req pluginapi.ExecutorRequest,
	stream bool,
) (*http.Request, error) {
	cleanModel := StripModelPrefix(req.Model, p.Config().ModelPrefix)
	cleanModel = strings.TrimPrefix(cleanModel, "models/")
	if cleanModel == "" && len(req.Payload) > 0 {
		if m := strings.TrimSpace(gjson.GetBytes(req.Payload, "model").String()); m != "" {
			cleanModel = StripModelPrefix(m, p.Config().ModelPrefix)
			cleanModel = strings.TrimPrefix(cleanModel, "models/")
		}
	}

	body := req.Payload
	format := strings.ToLower(strings.TrimSpace(req.Format))
	if format == "" {
		format = strings.ToLower(strings.TrimSpace(req.SourceFormat))
	}

	targetPath := "/v1/chat/completions"
	targetQuery := url.Values{}
	for k, v := range req.Query {
		targetQuery[k] = append([]string(nil), v...)
	}

	switch format {
	case "gemini":
		if cleanModel == "" {
			cleanModel = "gemini-2.5-flash"
		}
		lowerModel := strings.ToLower(cleanModel)
		switch {
		case gjson.GetBytes(body, "questions").Exists():
			targetPath = "/v1beta/decisions"
			body = patchJSONModelAndStream(body, cleanModel, true, false, false)
		case gjson.GetBytes(body, "instances").Exists():
			body = patchJSONModelAndStream(body, cleanModel, false, false, false)
			if strings.Contains(lowerModel, "veo") {
				targetPath = "/v1beta/models/" + cleanModel + ":predictLongRunning"
			} else {
				targetPath = "/v1beta/models/" + cleanModel + ":predict"
			}
		case gjson.GetBytes(body, "requests").Exists() && strings.Contains(lowerModel, "embedding"):
			body = patchJSONModelAndStream(body, cleanModel, false, false, false)
			targetPath = "/v1beta/models/" + cleanModel + ":batchEmbedContents"
		case gjson.GetBytes(body, "content").Exists() && strings.Contains(lowerModel, "embedding"):
			body = patchJSONModelAndStream(body, cleanModel, false, false, false)
			targetPath = "/v1beta/models/" + cleanModel + ":embedContent"
		case stream:
			body = patchJSONModelAndStream(body, cleanModel, false, false, false)
			targetPath = "/v1beta/models/" + cleanModel + ":streamGenerateContent"
			if targetQuery.Get("alt") == "" {
				targetQuery.Set("alt", "sse")
			}
		default:
			body = patchJSONModelAndStream(body, cleanModel, false, false, false)
			targetPath = "/v1beta/models/" + cleanModel + ":generateContent"
		}

	case "claude", "anthropic":
		targetPath = "/v1/messages"
		body = patchJSONModelAndStream(body, cleanModel, true, true, stream)

	case "openai-response", "openai_response", "responses", "openai-responses", "openai_responses":
		targetPath = "/v1/responses"
		body = patchJSONModelAndStream(body, cleanModel, true, true, stream)

	default:
		lowerModel := strings.ToLower(cleanModel)
		switch {
		case gjson.GetBytes(body, "questions").Exists():
			targetPath = "/v1beta/decisions"
			body = patchJSONModelAndStream(body, cleanModel, true, false, false)
		case gjson.GetBytes(body, "messages").Exists():
			targetPath = "/v1/chat/completions"
			body = patchJSONModelAndStream(body, cleanModel, true, true, stream)
		case gjson.GetBytes(body, "input").Exists() && strings.Contains(lowerModel, "embedding"):
			targetPath = "/v1/embeddings"
			body = patchJSONModelAndStream(body, cleanModel, false, false, false)
		case gjson.GetBytes(body, "prompt").Exists() && (strings.Contains(lowerModel, "imagen") || strings.Contains(lowerModel, "image")):
			targetPath = "/v1/images/generations"
			body = patchJSONModelAndStream(body, cleanModel, false, false, false)
		default:
			targetPath = "/v1/chat/completions"
			body = patchJSONModelAndStream(body, cleanModel, true, true, stream)
		}
	}

	fullURL := "http://aistudio2api.local" + targetPath
	if encodedQuery := targetQuery.Encode(); encodedQuery != "" {
		fullURL += "?" + encodedQuery
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, fullURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	for k, vals := range req.Headers {
		for _, v := range vals {
			httpReq.Header.Add(k, v)
		}
	}
	httpReq.Header.Del("Origin")
	if httpReq.Header.Get("Content-Type") == "" {
		httpReq.Header.Set("Content-Type", "application/json")
	}
	return httpReq, nil
}

func (p *Plugin) handleExecutorExecute(raw []byte) (any, error) {
	var req rpcExecutorRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, fmt.Errorf("decode executor.execute request: %w", err)
	}

	targetModel := resolveExecutorTargetModel(req.Model, req.Payload)
	reqCtx, rt, accountID, err := p.prepareExecutionContext(p.ctx, targetModel, req.StorageJSON, req.AuthMetadata, req.AuthAttributes)
	if err != nil {
		return nil, err
	}
	defer p.syncDegradedAccountIfChanged(req.HostCallbackID, rt, accountID)

	httpReq, err := p.buildInternalHTTPRequest(reqCtx, req.ExecutorRequest, false)
	if err != nil {
		return nil, err
	}

	rec := httptest.NewRecorder()
	rt.PublicHandler().ServeHTTP(rec, httpReq)
	resp := rec.Result()
	respBody := rec.Body.Bytes()

	if resp.StatusCode >= 400 {
		return nil, extractHTTPError(resp.StatusCode, resp.Header, respBody)
	}

	return pluginapi.ExecutorResponse{
		Payload: respBody,
		Headers: cloneHTTPHeader(resp.Header),
	}, nil
}

func (p *Plugin) handleExecutorExecuteStream(raw []byte) (any, error) {
	var req rpcExecutorRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, fmt.Errorf("decode executor.execute_stream request: %w", err)
	}

	targetModel := resolveExecutorTargetModel(req.Model, req.Payload)
	reqCtx, rt, accountID, err := p.prepareExecutionContext(p.ctx, targetModel, req.StorageJSON, req.AuthMetadata, req.AuthAttributes)
	if err != nil {
		return nil, err
	}

	streamCtx, cancelStream := context.WithCancel(reqCtx)
	httpReq, err := p.buildInternalHTTPRequest(streamCtx, req.ExecutorRequest, true)
	if err != nil {
		cancelStream()
		return nil, err
	}

	// If no StreamID was supplied (e.g. direct unit test call), collect chunks synchronously.
	if strings.TrimSpace(req.StreamID) == "" || p.hostCall == nil {
		defer cancelStream()
		defer p.syncDegradedAccountIfChanged(req.HostCallbackID, rt, accountID)
		rec := httptest.NewRecorder()
		rt.PublicHandler().ServeHTTP(rec, httpReq)
		resp := rec.Result()
		respBody := rec.Body.Bytes()
		if resp.StatusCode >= 400 {
			return nil, extractHTTPError(resp.StatusCode, resp.Header, respBody)
		}
		chunks := splitSSEChunks(respBody)
		return rpcExecutorStreamResponse{
			Headers: cloneHTTPHeader(resp.Header),
			Chunks:  chunks,
		}, nil
	}

	sw := newLiveStreamResponseWriter(p, req.StreamID, cancelStream)
	go func() {
		var panicErr *pluginabi.Error
		defer func() {
			if r := recover(); r != nil {
				msg := fmt.Sprintf("panic in stream handler: %v", r)
				panicErr = pluginabi.NewError("plugin_panic", msg, http.StatusInternalServerError)
				sw.WriteHeader(http.StatusInternalServerError)
				if sw.errBuf.Len() == 0 {
					_, _ = sw.errBuf.WriteString(msg)
				}
			}
			cancelStream()
			p.syncDegradedAccountIfChanged(req.HostCallbackID, rt, accountID)
			sw.finishHandler(panicErr)
		}()
		rt.PublicHandler().ServeHTTP(sw, httpReq)
	}()

	<-sw.headerReady
	if sw.statusCode >= 400 {
		<-sw.done
		return nil, extractHTTPError(sw.statusCode, sw.header, sw.errBuf.Bytes())
	}

	defer close(sw.rpcReturned)
	return rpcExecutorStreamResponse{
		Headers: sw.clonedHeaders(),
	}, nil
}

type liveStreamResponseWriter struct {
	plugin      *Plugin
	streamID    string
	cancel      context.CancelFunc
	header      http.Header
	statusCode  int
	headerOnce  sync.Once
	headerReady chan struct{}
	rpcReturned chan struct{}
	done        chan struct{}
	errBuf      bytes.Buffer
}

func newLiveStreamResponseWriter(plugin *Plugin, streamID string, cancel context.CancelFunc) *liveStreamResponseWriter {
	return &liveStreamResponseWriter{
		plugin:      plugin,
		streamID:    streamID,
		cancel:      cancel,
		header:      make(http.Header),
		statusCode:  http.StatusOK,
		headerReady: make(chan struct{}),
		rpcReturned: make(chan struct{}),
		done:        make(chan struct{}),
	}
}

func (w *liveStreamResponseWriter) Header() http.Header {
	return w.header
}

func (w *liveStreamResponseWriter) WriteHeader(statusCode int) {
	w.headerOnce.Do(func() {
		w.statusCode = statusCode
		close(w.headerReady)
	})
}

func (w *liveStreamResponseWriter) Write(p []byte) (int, error) {
	w.WriteHeader(http.StatusOK)
	if len(p) == 0 {
		return 0, nil
	}
	if w.statusCode >= 400 {
		return w.errBuf.Write(p)
	}
	<-w.rpcReturned
	if err := w.plugin.emitStreamChunk(w.streamID, p); err != nil {
		if w.cancel != nil {
			w.cancel()
		}
		return 0, err
	}
	return len(p), nil
}

func (w *liveStreamResponseWriter) Flush() {
	w.WriteHeader(http.StatusOK)
}

func (w *liveStreamResponseWriter) clonedHeaders() http.Header {
	return cloneHTTPHeader(w.header)
}

func (w *liveStreamResponseWriter) finishHandler(streamErr *pluginabi.Error) {
	w.WriteHeader(http.StatusOK)
	defer close(w.done)
	if w.statusCode >= 400 && streamErr == nil {
		return
	}
	select {
	case <-w.rpcReturned:
		_ = w.plugin.closeStream(w.streamID, streamErr)
	default:
		if streamErr != nil {
			// RPC hasn't returned yet because statusCode >= 400 was set before headerReady closed
			return
		}
		<-w.rpcReturned
		_ = w.plugin.closeStream(w.streamID, nil)
	}
}

func (p *Plugin) emitStreamChunk(streamID string, payload []byte) error {
	req := map[string]any{
		"stream_id": streamID,
		"payload":   payload,
	}
	_, err := p.callHost(pluginabi.MethodHostStreamEmit, req)
	return err
}

func (p *Plugin) closeStream(streamID string, streamErr *pluginabi.Error) error {
	req := map[string]any{
		"stream_id": streamID,
	}
	if streamErr != nil {
		req["error"] = streamErr.Message
		req["error_detail"] = streamErr
	}
	_, err := p.callHost(pluginabi.MethodHostStreamClose, req)
	return err
}

func (p *Plugin) handleExecutorCountTokens(raw []byte) (any, error) {
	var req rpcExecutorRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, fmt.Errorf("decode executor.count_tokens request: %w", err)
	}

	targetModel := resolveExecutorTargetModel(req.Model, req.Payload)
	reqCtx, rt, accountID, err := p.prepareExecutionContext(p.ctx, targetModel, req.StorageJSON, req.AuthMetadata, req.AuthAttributes)
	if err != nil {
		return nil, err
	}
	defer p.syncDegradedAccountIfChanged(req.HostCallbackID, rt, accountID)

	cleanModel := StripModelPrefix(req.Model, p.Config().ModelPrefix)
	cleanModel = strings.TrimPrefix(cleanModel, "models/")
	if cleanModel == "" {
		cleanModel = "gemini-2.5-flash"
	}

	format := strings.ToLower(strings.TrimSpace(req.Format))
	if format == "" {
		format = strings.ToLower(strings.TrimSpace(req.SourceFormat))
	}

	body := patchJSONModelAndStream(req.Payload, cleanModel, false, false, false)

	var targetPath string
	switch format {
	case "claude", "anthropic":
		targetPath = "/v1/messages/count_tokens"
	case "openai-response", "openai_response", "responses", "openai-responses", "openai_responses":
		targetPath = "/v1/responses/input_tokens"
	case "gemini":
		targetPath = "/v1beta/models/" + cleanModel + ":countTokens"
	default:
		// For OpenAI chat-completions, extract messages text and call rt.Manager.CountTokens
		text := extractMessagesText(body)
		if text == "" {
			text = string(body)
		}
		count, err := rt.Manager.CountTokens(reqCtx, aistudio.TokenCountRequest{
			Model: cleanModel,
			Contents: []aistudio.Content{
				{Role: aistudio.RoleUser, Parts: []aistudio.Part{{Text: text}}},
			},
		})
		if err != nil {
			return nil, extractHTTPError(http.StatusBadGateway, nil, []byte(err.Error()))
		}
		respPayload, _ := json.Marshal(map[string]any{
			"totalTokens":  count.InputTokens,
			"total_tokens": count.InputTokens,
			"input_tokens": count.InputTokens,
			"usage": map[string]any{
				"prompt_tokens": count.InputTokens,
				"total_tokens":  count.InputTokens,
			},
		})
		return pluginapi.ExecutorResponse{
			Payload: respPayload,
			Headers: http.Header{"Content-Type": []string{"application/json"}},
		}, nil
	}

	httpReq, err := http.NewRequestWithContext(reqCtx, http.MethodPost, "http://aistudio2api.local"+targetPath, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")

	rec := httptest.NewRecorder()
	rt.PublicHandler().ServeHTTP(rec, httpReq)
	resp := rec.Result()
	respBody := rec.Body.Bytes()
	if resp.StatusCode >= 400 {
		return nil, extractHTTPError(resp.StatusCode, resp.Header, respBody)
	}
	return pluginapi.ExecutorResponse{
		Payload: respBody,
		Headers: cloneHTTPHeader(resp.Header),
	}, nil
}

func (p *Plugin) handleExecutorHTTPRequest(raw []byte) (any, error) {
	var req rpcExecutorHTTPRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, fmt.Errorf("decode executor.http_request: %w", err)
	}

	method := strings.TrimSpace(req.Method)
	if method == "" {
		method = http.MethodGet
	}
	targetURL := strings.TrimSpace(req.URL)
	if targetURL == "" {
		targetURL = "/v1/models"
	}
	if strings.HasPrefix(targetURL, "/") {
		targetURL = "http://aistudio2api.local" + targetURL
	}
	if parsed, err := url.Parse(targetURL); err == nil {
		if parsed.Path == "/api" || strings.HasPrefix(parsed.Path, "/api/") {
			return pluginapi.ExecutorHTTPResponse{
				StatusCode: http.StatusForbidden,
				Headers:    http.Header{"Content-Type": []string{"application/json; charset=utf-8"}},
				Body:       []byte(`{"error":{"code":"forbidden","message":"management API routes are not accessible via executor"}}`),
			}, nil
		}
	}

	reqCtx, rt, accountID, err := p.prepareExecutionContext(p.ctx, "", req.StorageJSON, req.Metadata, req.Attributes)
	if err != nil {
		return nil, err
	}
	defer p.syncDegradedAccountIfChanged(req.HostCallbackID, rt, accountID)
	httpReq, err := http.NewRequestWithContext(reqCtx, method, targetURL, bytes.NewReader(req.Body))
	if err != nil {
		return nil, err
	}
	for k, vals := range req.Headers {
		for _, v := range vals {
			httpReq.Header.Add(k, v)
		}
	}
	httpReq.Header.Del("Origin")

	rec := httptest.NewRecorder()
	rt.PublicHandler().ServeHTTP(rec, httpReq)
	resp := rec.Result()

	return pluginapi.ExecutorHTTPResponse{
		StatusCode: resp.StatusCode,
		Headers:    cloneHTTPHeader(resp.Header),
		Body:       rec.Body.Bytes(),
	}, nil
}

func extractMessagesText(body []byte) string {
	var parts []string
	messages := gjson.GetBytes(body, "messages")
	if messages.IsArray() {
		messages.ForEach(func(_, msg gjson.Result) bool {
			content := msg.Get("content")
			if content.Type == gjson.String {
				parts = append(parts, content.String())
			} else if content.IsArray() {
				content.ForEach(func(_, item gjson.Result) bool {
					if t := item.Get("text").String(); t != "" {
						parts = append(parts, t)
					}
					return true
				})
			}
			return true
		})
	}
	return strings.Join(parts, "\n")
}

func extractHTTPError(statusCode int, headers http.Header, body []byte) *pluginabi.Error {
	code := "upstream_error"
	msg := strings.TrimSpace(string(body))
	if len(body) > 0 {
		if errMsg := strings.TrimSpace(gjson.GetBytes(body, "error.message").String()); errMsg != "" {
			msg = errMsg
		} else if errStr := strings.TrimSpace(gjson.GetBytes(body, "error").String()); errStr != "" && !strings.HasPrefix(errStr, "{") {
			msg = errStr
		}
		if errCode := strings.TrimSpace(gjson.GetBytes(body, "error.code").String()); errCode != "" {
			code = errCode
		} else if errType := strings.TrimSpace(gjson.GetBytes(body, "error.type").String()); errType != "" {
			code = errType
		}
	}
	if msg == "" {
		msg = http.StatusText(statusCode)
	}
	err := pluginabi.NewError(code, msg, statusCode)
	err.Retryable = statusCode == http.StatusTooManyRequests || statusCode >= http.StatusInternalServerError
	if headers != nil {
		if ra := strings.TrimSpace(headers.Get("Retry-After")); ra != "" {
			if secs, convErr := strconv.Atoi(ra); convErr == nil && secs > 0 {
				err.RetryAfterSeconds = secs
			} else if retryTime, timeErr := http.ParseTime(ra); timeErr == nil {
				if delta := int(time.Until(retryTime).Seconds()); delta > 0 {
					err.RetryAfterSeconds = delta
				}
			}
		}
	}
	return err
}

func splitSSEChunks(raw []byte) []pluginapi.ExecutorStreamChunk {
	if len(raw) == 0 {
		return nil
	}
	lines := bytes.SplitAfter(raw, []byte("\n\n"))
	chunks := make([]pluginapi.ExecutorStreamChunk, 0, len(lines))
	for _, line := range lines {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		chunks = append(chunks, pluginapi.ExecutorStreamChunk{Payload: bytes.Clone(line)})
	}
	return chunks
}

func cloneHTTPHeader(in http.Header) http.Header {
	if len(in) == 0 {
		return nil
	}
	out := make(http.Header, len(in))
	for k, vals := range in {
		out[k] = append([]string(nil), vals...)
	}
	return out
}
