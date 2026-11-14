package aistudio

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestPooledService_AssistantModelsOptInGate(t *testing.T) {
	pool := NewAccountPool([]*Account{{ID: "a@example.com", Config: DefaultAccountConfig("a@example.com")}}, 2)
	if err := pool.SetCatalog("a@example.com", BenefitTierPro, []Model{
		{ID: "gemini-3.8-flash", Name: "Gemini 3.8 Flash", Methods: []string{"generateContent"}, Capabilities: map[string]bool{"chat_model": true}},
		{ID: "gemini-3.8-flash-build", Name: "Gemini 3.8 Flash (Build)", Methods: []string{"generateContent"}, Capabilities: map[string]bool{"chat_model": true, "assistant_route": true}},
	}); err != nil {
		t.Fatalf("SetCatalog failed: %v", err)
	}

	svc := NewPooledService(pool, NewClient(ClientOptions{}))
	req := GenerateRequest{
		Model:    "gemini-3.8-flash-build",
		Contents: []Content{{Role: RoleUser, Parts: []Part{{Text: "hello"}}}},
	}

	if err := svc.ValidateGenerateRequest(req); !errors.Is(err, ErrModelNotFound) || !strings.Contains(err.Error(), "enable_assistant_models") {
		t.Fatalf("expected ErrModelNotFound mentioning enable_assistant_models, got %v", err)
	}
	if _, err := svc.Generate(context.Background(), req); !errors.Is(err, ErrModelNotFound) {
		t.Fatalf("expected Generate ErrModelNotFound when assistant disabled, got %v", err)
	}
	if _, err := svc.CountTokens(context.Background(), TokenCountRequest{Model: req.Model, Contents: req.Contents}); !errors.Is(err, ErrModelNotFound) {
		t.Fatalf("expected CountTokens ErrModelNotFound when assistant disabled, got %v", err)
	}

	pool.SetAssistantEnabled(true)
	if err := svc.ValidateGenerateRequest(req); err != nil {
		t.Fatalf("expected ValidateGenerateRequest to succeed when assistant enabled, got %v", err)
	}
}
