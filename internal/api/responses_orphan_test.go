package api

import (
	"strings"
	"testing"
)

func TestResponsesContents_OrphanToolOutputGracefulFallback(t *testing.T) {
	input := []byte(`[
		{"type":"message","role":"user","content":"hello"},
		{"type":"function_call_output","call_id":"call_orphan","output":"{\"result\":\"subagent task completed\"}"}
	]`)
	contents, _, err := responsesContents(splitResponsesInput(input))
	if err != nil {
		t.Fatalf("unexpected error parsing responses contents: %v", err)
	}
	if len(contents) != 2 {
		t.Fatalf("expected 2 contents, got %d", len(contents))
	}
	if contents[1].Role != "user" {
		t.Errorf("expected orphan tool output to degrade to user role, got %q", contents[1].Role)
	}
	if len(contents[1].Parts) != 1 || !strings.Contains(contents[1].Parts[0].Text, "subagent task completed") {
		t.Errorf("expected text part containing 'subagent task completed', got: %#v", contents[1].Parts)
	}
}

func TestResponsesContents_PairedFunctionCallWithID(t *testing.T) {
	input := []byte(`[
		{"type":"message","role":"user","content":"run command"},
		{"type":"function_call","id":"call_123","name":"exec_command","arguments":"{\"cmd\":\"dir\"}"},
		{"type":"function_call_output","call_id":"call_123","output":"{\"result\":\"file.txt\"}"}
	]`)
	contents, _, err := responsesContents(splitResponsesInput(input))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(contents) != 3 {
		t.Fatalf("expected 3 contents, got %d", len(contents))
	}
	if contents[1].Role != "assistant" || contents[1].Parts[0].FunctionCall == nil || contents[1].Parts[0].FunctionCall.ID != "call_123" {
		t.Errorf("expected functionCall.ID == 'call_123', got: %#v", contents[1].Parts[0])
	}
	if contents[2].Role != "tool" || contents[2].Parts[0].FunctionResult == nil || contents[2].Parts[0].FunctionResult.Name != "exec_command" {
		t.Errorf("expected functionResult.Name == 'exec_command', got: %#v", contents[2].Parts[0])
	}
}

func TestResponsesContents_OrphanToolOutputWithPendingSiblingCall(t *testing.T) {
	input := []byte(`[
		{"type":"message","role":"user","content":"parallel work"},
		{"type":"function_call","id":"call_active","name":"exec_command","arguments":"{\"cmd\":\"ls\"}"},
		{"type":"function_call_output","call_id":"call_orphan_other","output":"{\"result\":\"orphan result\"}"},
		{"type":"function_call_output","call_id":"call_active","output":"{\"result\":\"ls output\"}"}
	]`)
	contents, _, err := responsesContents(splitResponsesInput(input))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(contents) != 4 {
		t.Fatalf("expected 4 contents, got %d", len(contents))
	}
	if contents[2].Role != "user" || len(contents[2].Parts) != 1 || !strings.Contains(contents[2].Parts[0].Text, "orphan result") {
		t.Errorf("expected orphan output to degrade to user text part even when sibling call is pending, got: %#v", contents[2])
	}
	if contents[3].Role != "tool" || contents[3].Parts[0].FunctionResult == nil || contents[3].Parts[0].FunctionResult.Name != "exec_command" {
		t.Errorf("expected active call output to pair with exec_command, got: %#v", contents[3])
	}
}
