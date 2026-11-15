package aistudio

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

type scriptedProtectedTransport struct {
	attempts []func(ctx context.Context, req GenerateRequest, rpc RPCRequest) (*RPCResponse, error)
	calls    int
}

func (s *scriptedProtectedTransport) DoProtected(ctx context.Context, req GenerateRequest, rpc RPCRequest) (*RPCResponse, error) {
	idx := s.calls
	s.calls++
	if idx >= len(s.attempts) {
		idx = len(s.attempts) - 1
	}
	return s.attempts[idx](ctx, req, rpc)
}

func makeWirePart(text string, thought bool, signature string) []any {
	part := make([]any, 15)
	if text != "" {
		part[1] = text
	}
	if thought {
		part[12] = 1
	}
	if signature != "" {
		part[14] = signature
	}
	return part
}

func makeGenerateStreamBody(t *testing.T, frames ...[]any) io.ReadCloser {
	t.Helper()
	raw, err := json.Marshal([]any{frames, []any{0}})
	if err != nil {
		t.Fatalf("marshal stream frames: %v", err)
	}
	return io.NopCloser(strings.NewReader(string(raw)))
}

func makeWireFrame(parts [][]any, finishCode any) []any {
	content := []any{parts, "model"}
	candidate := []any{content}
	if finishCode != nil {
		candidate = append(candidate, finishCode)
	}
	return []any{[]any{candidate}}
}

func TestGenerateMaxRegenerationsAndHeldEvents(t *testing.T) {
	model := Model{
		ID:               "gemini-3.8-flash",
		Name:             "Gemini 3.8 Flash",
		Methods:          []string{"generateContent"},
		InputTokenLimit:  8192,
		OutputTokenLimit: 8192,
		Capabilities:     map[string]bool{"chat_model": true, "thinking": true},
	}
	entry := modelEntry{model: model, defaults: GenerationDefaults{MaxOutputTokens: 8192}}

	t.Run("max_regenerations=0 disables retry on abnormal finish", func(t *testing.T) {
		transport := &scriptedProtectedTransport{
			attempts: []func(context.Context, GenerateRequest, RPCRequest) (*RPCResponse, error){
				func(context.Context, GenerateRequest, RPCRequest) (*RPCResponse, error) {
					return &RPCResponse{
						StatusCode: http.StatusOK,
						Header:     http.Header{"Content-Type": {JSONProtobufContentType}},
						Body:       makeGenerateStreamBody(t, makeWireFrame([][]any{makeWirePart("plan", true, "sig_fail")}, 19)),
					}, nil
				},
			},
		}
		client := NewClient(ClientOptions{Protected: transport})
		client.setModelCatalog("acct@example.com", modelCatalog{
			models:  []Model{model},
			entries: map[string]modelEntry{model.ID: entry},
		})

		zero := 0
		observed := 0
		ctx := ContextWithRegenerationObserver(context.Background(), func(attempt, max int, reason string) {
			observed++
		})
		events, err := client.Generate(ctx, GenerateRequest{
			AccountID:        "acct@example.com",
			Model:            model.ID,
			Contents:         []Content{{Role: RoleUser, Parts: []Part{{Text: "hi"}}}},
			Config:           GenerationConfig{HideThinking: true},
			MaxRegenerations: &zero,
		})
		if err != nil {
			t.Fatalf("Generate failed: %v", err)
		}
		var gotErr error
		var gotTexts []string
		var gotSigs []string
		for ev := range events {
			if ev.Kind == EventError {
				gotErr = ev.Err
			}
			if ev.Text != "" {
				gotTexts = append(gotTexts, ev.Text)
			}
			if ev.ThoughtSignature != "" {
				gotSigs = append(gotSigs, ev.ThoughtSignature)
			}
		}
		if transport.calls != 1 {
			t.Fatalf("expected 1 upstream call when MaxRegenerations=0, got %d", transport.calls)
		}
		if observed != 0 {
			t.Fatalf("expected 0 regeneration observer calls, got %d", observed)
		}
		var abn *AbnormalFinishError
		if !errors.As(gotErr, &abn) {
			t.Fatalf("expected AbnormalFinishError, got %v", gotErr)
		}
		if len(gotTexts) != 0 || len(gotSigs) != 0 {
			t.Fatalf("expected no leaked text or signature before failure, got texts=%v sigs=%v", gotTexts, gotSigs)
		}
	})

	t.Run("retries up to MaxRegenerations and drops pre-commit held events from failed attempts", func(t *testing.T) {
		transport := &scriptedProtectedTransport{
			attempts: []func(context.Context, GenerateRequest, RPCRequest) (*RPCResponse, error){
				func(context.Context, GenerateRequest, RPCRequest) (*RPCResponse, error) {
					// Attempt 1: interrupted stream without finish frame, preceded by hidden thinking signature
					return &RPCResponse{
						StatusCode: http.StatusOK,
						Header:     http.Header{"Content-Type": {JSONProtobufContentType}},
						Body:       makeGenerateStreamBody(t, makeWireFrame([][]any{makeWirePart("stale1", true, "sig_stale_1")}, nil)),
					}, nil
				},
				func(context.Context, GenerateRequest, RPCRequest) (*RPCResponse, error) {
					// Attempt 2: MALFORMED_RESPONSE (finish code 19) with unary thinking held before text
					return &RPCResponse{
						StatusCode: http.StatusOK,
						Header:     http.Header{"Content-Type": {JSONProtobufContentType}},
						Body:       makeGenerateStreamBody(t, makeWireFrame([][]any{makeWirePart("stale2", true, "sig_stale_2")}, 19)),
					}, nil
				},
				func(context.Context, GenerateRequest, RPCRequest) (*RPCResponse, error) {
					// Attempt 3: succeeds cleanly
					return &RPCResponse{
						StatusCode: http.StatusOK,
						Header:     http.Header{"Content-Type": {JSONProtobufContentType}},
						Body: makeGenerateStreamBody(t,
							makeWireFrame([][]any{makeWirePart("good_thought", true, "sig_good")}, nil),
							makeWireFrame([][]any{makeWirePart("final_answer", false, "")}, 1),
						),
					}, nil
				},
			},
		}
		client := NewClient(ClientOptions{Protected: transport})
		client.setModelCatalog("acct@example.com", modelCatalog{
			models:  []Model{model},
			entries: map[string]modelEntry{model.ID: entry},
		})

		two := 2
		type regenLog struct {
			attempt, max int
			reason       string
		}
		var logs []regenLog
		ctx := ContextWithRegenerationObserver(context.Background(), func(attempt, max int, reason string) {
			logs = append(logs, regenLog{attempt: attempt, max: max, reason: reason})
		})
		events, err := client.Generate(ctx, GenerateRequest{
			AccountID:        "acct@example.com",
			Model:            model.ID,
			Contents:         []Content{{Role: RoleUser, Parts: []Part{{Text: "hi"}}}},
			Unary:            true,
			Config:           GenerationConfig{HideThinking: true},
			MaxRegenerations: &two,
		})
		if err != nil {
			t.Fatalf("Generate failed: %v", err)
		}
		var sigs []string
		var texts []string
		var finishReason string
		for ev := range events {
			if ev.Kind == EventError {
				t.Fatalf("unexpected EventError: %v", ev.Err)
			}
			if ev.ThoughtSignature != "" {
				sigs = append(sigs, ev.ThoughtSignature)
			}
			if ev.Kind == EventText {
				texts = append(texts, ev.Text)
			}
			if ev.Kind == EventFinish {
				finishReason = ev.FinishReason
			}
		}
		if transport.calls != 3 {
			t.Fatalf("expected 3 upstream calls, got %d", transport.calls)
		}
		if len(logs) != 2 || logs[0].attempt != 1 || logs[0].max != 2 || logs[1].attempt != 2 || logs[1].max != 2 || logs[0].reason == "" || logs[1].reason == "" {
			t.Fatalf("unexpected regeneration observer logs: %+v", logs)
		}
		if strings.Join(texts, "") != "final_answer" {
			t.Fatalf("expected text 'final_answer', got %v", texts)
		}
		if len(sigs) != 1 || sigs[0] != "sig_good" {
			t.Fatalf("expected only winning attempt thoughtSignature [sig_good], got %v", sigs)
		}
		if finishReason != "stop" {
			t.Fatalf("expected finishReason=stop, got %q", finishReason)
		}
	})

	t.Run("PooledService propagates MaxRegenerations to Client.Generate", func(t *testing.T) {
		transport := &scriptedProtectedTransport{
			attempts: []func(context.Context, GenerateRequest, RPCRequest) (*RPCResponse, error){
				func(context.Context, GenerateRequest, RPCRequest) (*RPCResponse, error) {
					return &RPCResponse{
						StatusCode: http.StatusOK,
						Header:     http.Header{"Content-Type": {JSONProtobufContentType}},
						Body:       makeGenerateStreamBody(t, makeWireFrame([][]any{makeWirePart("", false, "")}, 19)),
					}, nil
				},
			},
		}
		pool := NewAccountPool([]*Account{{ID: "acct@example.com", State: AccountReady, Config: DefaultAccountConfig("acct@example.com")}}, 2)
		if err := pool.SetCatalog("acct@example.com", BenefitTierPro, []Model{model}); err != nil {
			t.Fatalf("SetCatalog failed: %v", err)
		}
		client := NewClient(ClientOptions{Protected: transport})
		svc := NewPooledService(pool, client)
		client.setModelCatalog("acct@example.com", modelCatalog{
			models:  []Model{model},
			entries: map[string]modelEntry{model.ID: entry},
		})
		zero := 0
		svc.MaxRegenerations = &zero

		events, err := svc.Generate(context.Background(), GenerateRequest{
			Model:    model.ID,
			Contents: []Content{{Role: RoleUser, Parts: []Part{{Text: "hi"}}}},
		})
		if err != nil {
			t.Fatalf("PooledService.Generate failed: %v", err)
		}
		var gotErr error
		for ev := range events {
			if ev.Kind == EventError {
				gotErr = ev.Err
			}
		}
		if transport.calls != 1 {
			t.Fatalf("expected PooledService.MaxRegenerations=0 to stop after 1 call, got %d", transport.calls)
		}
		var abn *AbnormalFinishError
		if !errors.As(gotErr, &abn) {
			t.Fatalf("expected AbnormalFinishError, got %v", gotErr)
		}
	})
}
