package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"
)

// interactionWebhookTimeout 是单次 webhook 投递请求的超时时长
const interactionWebhookTimeout = 10 * time.Second
// interactionWebhookWindow 是 webhook 事件投递失败后继续重试的时长
const interactionWebhookWindow = 24 * time.Hour

// interactionWebhookConfig 是请求级 webhook 的地址与随事件返回的用户元数据
type interactionWebhookConfig struct {
	URIs         []string        `json:"uris"`
	UserMetadata json.RawMessage `json:"user_metadata"`
}

// interactionWebhookEvent 是按官方字段顺序编码的 webhook 事件信封
type interactionWebhookEvent struct {
	Type         string                 `json:"type"`
	Version      string                 `json:"version"`
	Timestamp    string                 `json:"timestamp"`
	Data         interactionWebhookData `json:"data"`
	UserMetadata json.RawMessage        `json:"user_metadata,omitempty"`
}

// interactionWebhookData 是事件指向的交互 ID 与失败时的错误
type interactionWebhookData struct {
	ID           string `json:"id"`
	ErrorCode    string `json:"error_code,omitempty"`
	ErrorMessage string `json:"error_message,omitempty"`
}

// interactionWebhookEvents 是交互终态对应的 webhook 事件类型
var interactionWebhookEvents = map[string]string{
	"completed": "interaction.completed", "incomplete": "interaction.completed", "requires_action": "interaction.requires_action",
	"failed": "interaction.failed", "cancelled": "interaction.cancelled",
}

// webhookOutboundValidator 返回受管理员白名单与公网 IP 策略约束且禁止重定向的 Webhook 出站校验器
func (s *server) webhookOutboundValidator() outboundValidator {
	raw := ""
	if s != nil && s.config.AllowedWebhookHosts != nil {
		raw = s.config.AllowedWebhookHosts()
	}
	var egressProxy func() string
	if s != nil {
		egressProxy = s.config.EgressProxy
	}
	return outboundValidator{
		kind:             "webhook",
		configKey:        "allowed_webhook_hosts",
		allowlist:        ParseOutboundAllowlist(raw),
		requireAllowlist: true,
		allowRedirects:   false,
		egressProxy:      egressProxy,
	}
}

// validate 要求请求为后台交互且每个地址均通过管理员白名单与公网 IP 校验
func (config interactionWebhookConfig) validate(ctx context.Context, background bool, validator outboundValidator) error {
	if !background {
		return fmt.Errorf("webhook_config requires background to be true")
	}
	for _, uri := range config.URIs {
		if _, _, err := validator.validateURL(ctx, uri); err != nil {
			return fmt.Errorf("webhook_config.uris %q: %w", uri, err)
		}
	}
	return nil
}

// sendInteractionWebhook 向每个地址发送交互终态事件，failure 为后台请求写出的 Gemini 错误
func (s *server) sendInteractionWebhook(config interactionWebhookConfig, id, status string, failure []byte) {
	event := interactionWebhookEvent{Type: interactionWebhookEvents[status], Version: "v1", Timestamp: time.Now().UTC().Format(time.RFC3339), Data: interactionWebhookData{ID: id}}
	var written struct {
		Error struct {
			Message string `json:"message"`
			Status  string `json:"status"`
		} `json:"error"`
	}
	if status == "failed" && json.Unmarshal(failure, &written) == nil {
		event.Data.ErrorCode, event.Data.ErrorMessage = written.Error.Status, written.Error.Message
	}
	if rawJSONConfigured(config.UserMetadata) {
		event.UserMetadata = config.UserMetadata
	}
	body, _ := json.Marshal(event)
	messageID := newID("msg")
	for _, uri := range config.URIs {
		go s.deliverInteractionWebhook(uri, messageID, body)
	}
}

// deliverInteractionWebhook 以 webhook-id 与 webhook-timestamp 请求头 POST 事件，每次投递前重新按最新白名单与公网 IP 校验并禁止重定向，非 2xx 时指数退避重试，遇安全策略拒绝立即停止
func (s *server) deliverInteractionWebhook(uri, messageID string, body []byte) {
	deadline := time.Now().Add(interactionWebhookWindow)
	for delay := time.Second; ; delay = min(delay*2, time.Hour) {
		validator := s.webhookOutboundValidator()
		reqCtx, cancel := context.WithTimeout(context.Background(), interactionWebhookTimeout)
		if _, _, err := validator.validateURL(reqCtx, uri); err != nil {
			cancel()
			if errors.Is(err, errOutboundBlocked) {
				return
			}
		} else {
			client, clientErr := validator.newHTTPClient(interactionWebhookTimeout, nil)
			if clientErr == nil {
				request, reqErr := http.NewRequestWithContext(reqCtx, http.MethodPost, uri, bytes.NewReader(body))
				if reqErr == nil {
					request.Header.Set("Content-Type", "application/json")
					request.Header.Set("webhook-id", messageID)
					request.Header.Set("webhook-timestamp", strconv.FormatInt(time.Now().Unix(), 10))
					response, err := client.Do(request)
					if err == nil {
						response.Body.Close()
						cancel()
						if response.StatusCode/100 == 2 {
							return
						}
					} else {
						cancel()
						if errors.Is(err, errOutboundBlocked) {
							return
						}
					}
				} else {
					cancel()
				}
			} else {
				cancel()
			}
		}
		if time.Now().Add(delay).After(deadline) {
			return
		}
		time.Sleep(delay)
	}
}
