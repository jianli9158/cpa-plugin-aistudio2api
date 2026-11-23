package aistudio

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/camoufoxnative"
)

func TestSillyTavernHelpersAndCooldownReset(t *testing.T) {
	base := GenerateRequest{
		Model:              "gemini-3.8-flash",
		UserMessageRewrite: userMessageRewriteTraditional,
		Contents: []Content{
			{Role: RoleUser, Parts: []Part{{Text: "请把这段简体中文转换，保留 ASCII 123"}}},
			{Role: RoleAssistant, Parts: []Part{{Text: "简体助手历史保持原样"}}},
		},
	}

	trad, err := applyChineseRewrite(base)
	if err != nil {
		t.Fatalf("applyChineseRewrite(traditional) failed: %v", err)
	}
	if got := trad.Contents[0].Parts[0].Text; got == base.Contents[0].Parts[0].Text || !strings.Contains(got, "ASCII 123") {
		t.Fatalf("unexpected traditional rewrite result: %q", got)
	}
	if got := trad.Contents[1].Parts[0].Text; got != "简体助手历史保持原样" {
		t.Fatalf("assistant history must not be rewritten, got %q", got)
	}
	if len(trad.System) == 0 || !strings.Contains(trad.System[len(trad.System)-1], "Simplified Chinese") {
		t.Fatalf("expected Simplified Chinese reply instruction in system, got %v", trad.System)
	}

	martianReq := base
	martianReq.UserMessageRewrite = userMessageRewriteMartian
	martian, err := applyChineseRewrite(martianReq)
	if err != nil {
		t.Fatalf("applyChineseRewrite(martian) failed: %v", err)
	}
	if got := martian.Contents[0].Parts[0].Text; got == base.Contents[0].Parts[0].Text || got == trad.Contents[0].Parts[0].Text {
		t.Fatalf("expected martian rewrite to differ from source and traditional, got %q", got)
	}

	entry := modelEntry{
		model:    Model{ID: "gemini-3.8-flash", Capabilities: map[string]bool{"function_declarations": true}},
		defaults: GenerationDefaults{Thinking: true, ThinkingLevel: true},
	}
	guardReq := GenerateRequest{
		Model:         "gemini-3.8-flash",
		ThinkingGuard: true,
		Config:        GenerationConfig{ReasoningEffort: "high"},
	}
	if !thinkingGuardEligible(guardReq, entry) {
		t.Fatal("expected thinkingGuardEligible to return true")
	}
	guarded := applyThinkingGuard(guardReq, entry)
	if guarded.Config.ReasoningEffort != "low" {
		t.Fatalf("expected ReasoningEffort clamped to low, got %q", guarded.Config.ReasoningEffort)
	}

	replyReq := GenerateRequest{Model: "gemini-3.8-flash", ReplyTool: true}
	if !replyToolEligible(replyReq, entry, false, false) {
		t.Fatal("expected replyToolEligible to return true")
	}
	applied := applyReplyTool(replyReq)
	if len(applied.Tools.Functions) != 1 || applied.Tools.Functions[0].Name != replyToolName {
		t.Fatalf("expected final function declaration, got %+v", applied.Tools.Functions)
	}
	if text, ok := replyToolText(json.RawMessage(`{"content":"hello"}`)); !ok || text != "hello" {
		t.Fatalf("unexpected replyToolText result: %q, %v", text, ok)
	}

	if d, ok := usageLimitReset("quota resets in 1 day, 2 hours, 3 minutes and 4 seconds"); !ok || d != (24*time.Hour+2*time.Hour+3*time.Minute+4*time.Second) {
		t.Fatalf("unexpected usageLimitReset duration: %v, ok=%v", d, ok)
	}
}

func TestClearAccountStatePreservesPerModelCooldowns(t *testing.T) {
	store := NewAccountStore(t.TempDir())
	state := StorageState{
		Cookies: []StateCookie{
			{Name: "SAPISID", Value: "test-sapisid", Domain: ".google.com", Path: "/", Expires: -1, Secure: true},
			{Name: "__Secure-1PSID", Value: "test-psid", Domain: ".google.com", Path: "/", Expires: -1, Secure: true},
		},
	}
	accCfg := DefaultAccountConfig("cooldown-preserve@example.com")
	accCfg.Locale = "en-US"
	accCfg.Timezone = "UTC"
	acc, publishLease, err := store.Create(accCfg, state)
	if err != nil {
		t.Fatalf("store.Create failed: %v", err)
	}
	if err := publishLease.Release(); err != nil {
		t.Fatalf("publishLease.Release failed: %v", err)
	}
	pool := NewAccountPool([]*Account{acc}, 2)

	if err := pool.MarkRestricted(acc.ID, &camoufoxnative.AccessDenial{Err: camoufoxnative.ErrAccountVerification, URL: camoufoxnative.AccountVerificationURL}); err != nil {
		t.Fatalf("MarkRestricted failed: %v", err)
	}
	checkedAt := time.Now().UTC()
	until := checkedAt.Add(30 * time.Minute)
	if err := pool.MarkCooldownIfGeneration(acc.ID, globalCooldownKey, 0, checkedAt, until, "global test"); err != nil {
		t.Fatalf("MarkCooldownIfGeneration(global) failed: %v", err)
	}
	if err := pool.MarkCooldownIfGeneration(acc.ID, "gemini-3.8-flash", 0, checkedAt, until, "model quota test"); err != nil {
		t.Fatalf("MarkCooldownIfGeneration(model) failed: %v", err)
	}

	if err := pool.ClearAccountState(acc.ID); err != nil {
		t.Fatalf("ClearAccountState failed: %v", err)
	}

	statuses := pool.Status()
	if len(statuses) != 1 {
		t.Fatalf("expected 1 status, got %d", len(statuses))
	}
	if statuses[0].State != AccountReady {
		t.Fatalf("expected AccountReady after ClearAccountState, got %s", statuses[0].State)
	}
	if _, hasGlobal := statuses[0].Cooldowns[globalCooldownKey]; hasGlobal {
		t.Fatal("expected global cooldown to be cleared")
	}
	if _, hasModel := statuses[0].Cooldowns["gemini-3.8-flash"]; !hasModel {
		t.Fatal("expected per-model cooldown to be preserved by ClearAccountState")
	}
}
