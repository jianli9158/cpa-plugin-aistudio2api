package api

import (
	"encoding/base64"
	"testing"
)

func TestResponsesContents_CPAGeminiCarrierDecoding(t *testing.T) {
	t.Run("next function carrier exact vector", func(t *testing.T) {
		input := []byte(`[
			{"type":"message","role":"user","content":"check files"},
			{"type":"reasoning","id":"rs_1","encrypted_content":"cpa-gemini-responses-carrier-v1:next:function:WVdKag"},
			{"type":"function_call","id":"call_1","name":"list_dir","arguments":"{\"path\":\".\"}"},
			{"type":"function_call_output","call_id":"call_1","output":"{\"result\":\"ok\"}"}
		]`)
		contents, _, err := responsesContents(splitResponsesInput(input))
		if err != nil {
			t.Fatalf("responsesContents failed: %v", err)
		}
		if len(contents) != 3 {
			t.Fatalf("expected 3 contents, got %d", len(contents))
		}
		call := contents[1].Parts[0].FunctionCall
		if call == nil || call.ThoughtSignature != "YWJj" {
			t.Fatalf("expected ThoughtSignature 'YWJj', got %#v", contents[1].Parts)
		}
	})

	t.Run("next text carrier attaches to assistant text and not function_call", func(t *testing.T) {
		rawSig := "sig_text_abc"
		encoded := "cpa-gemini-responses-carrier-v1:next:text:" + base64.RawStdEncoding.EncodeToString([]byte(rawSig))
		input := []byte(`[
			{"type":"message","role":"user","content":"hi"},
			{"type":"reasoning","encrypted_content":"` + encoded + `"},
			{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hello there"}]}
		]`)
		contents, _, err := responsesContents(splitResponsesInput(input))
		if err != nil {
			t.Fatalf("responsesContents failed: %v", err)
		}
		if len(contents) != 2 || contents[1].Parts[0].ThoughtSignature != rawSig {
			t.Fatalf("expected assistant text part to carry decoded signature %q, got %#v", rawSig, contents)
		}

		mismatchInput := []byte(`[
			{"type":"message","role":"user","content":"run tool"},
			{"type":"reasoning","encrypted_content":"` + encoded + `"},
			{"type":"function_call","id":"call_2","name":"ping","arguments":"{}"}
		]`)
		contents, _, err = responsesContents(splitResponsesInput(mismatchInput))
		if err != nil {
			t.Fatalf("responsesContents failed: %v", err)
		}
		if contents[1].Parts[0].FunctionCall.ThoughtSignature != "" {
			t.Fatalf("expected next:text carrier not to bind to function_call, got %q", contents[1].Parts[0].FunctionCall.ThoughtSignature)
		}
	})

	t.Run("previous carrier attaches backward without leaking forward", func(t *testing.T) {
		rawSig := "sig_prev_fn"
		encoded := "cpa-gemini-responses-carrier-v1:previous:function:" + base64.RawStdEncoding.EncodeToString([]byte(rawSig))
		input := []byte(`[
			{"type":"message","role":"user","content":"first"},
			{"type":"function_call","id":"call_prev","name":"tool_a","arguments":"{}"},
			{"type":"reasoning","encrypted_content":"` + encoded + `"},
			{"type":"function_call_output","call_id":"call_prev","output":"{\"result\":\"done\"}"},
			{"type":"function_call","id":"call_next","name":"tool_b","arguments":"{}"}
		]`)
		contents, _, err := responsesContents(splitResponsesInput(input))
		if err != nil {
			t.Fatalf("responsesContents failed: %v", err)
		}
		if got := contents[1].Parts[0].FunctionCall.ThoughtSignature; got != rawSig {
			t.Fatalf("expected previous function_call to receive %q, got %q", rawSig, got)
		}
		if got := contents[3].Parts[0].FunctionCall.ThoughtSignature; got != "" {
			t.Fatalf("expected following function_call not to receive previous signature, got %q", got)
		}
	})

	t.Run("malformed carrier stripped and bare signature preserved", func(t *testing.T) {
		for _, bad := range []string{
			"cpa-gemini-responses-carrier-v1:invalid:function:WVdKag",
			"cpa-gemini-responses-carrier-v1:next:badtarget:WVdKag",
			"cpa-gemini-responses-carrier-v1:next:function:!!!notbase64!!!",
			"cpa-gemini-responses-carrier-v1:next:function:" + base64.RawStdEncoding.EncodeToString([]byte("cpa-gemini-responses-carrier-v1:next:function:WVdKag")),
		} {
			input := []byte(`[
				{"type":"message","role":"user","content":"hi"},
				{"type":"reasoning","encrypted_content":"` + bad + `"},
				{"type":"function_call","id":"call_x","name":"tool_x","arguments":"{}"}
			]`)
			contents, _, err := responsesContents(splitResponsesInput(input))
			if err != nil {
				t.Fatalf("unexpected error for %q: %v", bad, err)
			}
			if got := contents[1].Parts[0].FunctionCall.ThoughtSignature; got != "" {
				t.Fatalf("expected corrupt carrier %q to be stripped, got %q", bad, got)
			}
		}

		bareInput := []byte(`[
			{"type":"message","role":"user","content":"hi"},
			{"type":"reasoning","encrypted_content":"YWJjZGVm"},
			{"type":"function_call","id":"call_bare","name":"tool_bare","arguments":"{}"}
		]`)
		contents, _, err := responsesContents(splitResponsesInput(bareInput))
		if err != nil {
			t.Fatalf("unexpected error for bare signature: %v", err)
		}
		if got := contents[1].Parts[0].FunctionCall.ThoughtSignature; got != "YWJjZGVm" {
			t.Fatalf("expected bare signature preserved, got %q", got)
		}
	})
}
