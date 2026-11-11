package aistudio

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
)

func TestAssistantCallsRepairsTrailingCommasAndControlChars(t *testing.T) {
	t.Run("trailing commas and unescaped control characters", func(t *testing.T) {
		body := "{\n" +
			"  \"name\": \"write_file\",\n" +
			"  \"arguments\": {\n" +
			"    \"path\": \"C:\\Users\\demo\\work.txt\",\n" +
			"    \"content\": \"line1\nline2\tindented\",\n" +
			"    \"tags\": [\"a\", \"b\", ],\n" +
			"  },\n" +
			"}\n"
		events, ok := assistantCalls(body, true, []string{"write_file"})
		if !ok || len(events) != 1 || events[0].Kind != EventToolCall || events[0].FunctionCall == nil {
			t.Fatalf("expected 1 decoded tool call, got ok=%v events=%#v", ok, events)
		}
		if events[0].FunctionCall.Name != "write_file" {
			t.Fatalf("unexpected function name: %q", events[0].FunctionCall.Name)
		}
		var args map[string]any
		if err := json.Unmarshal(events[0].FunctionCall.Arguments, &args); err != nil {
			t.Fatalf("decoded arguments invalid JSON: %v (%s)", err, string(events[0].FunctionCall.Arguments))
		}
		if args["path"] != `C:\Users\demo\work.txt` {
			t.Fatalf("unexpected path: %#v", args["path"])
		}
		if args["content"] != "line1\nline2\tindented" {
			t.Fatalf("unexpected content: %#v", args["content"])
		}
	})

	t.Run("array with trailing commas and string commas preserved", func(t *testing.T) {
		body := `[
			{"name": "echo", "arguments": {"text": "keep, } and \", ]", "nested": {"x": 1,},},},
		]`
		events, ok := assistantCalls(body, false, []string{"echo"})
		if !ok || len(events) != 1 || events[0].FunctionCall == nil {
			t.Fatalf("expected 1 decoded tool call, got ok=%v events=%#v", ok, events)
		}
		var args map[string]any
		if err := json.Unmarshal(events[0].FunctionCall.Arguments, &args); err != nil {
			t.Fatalf("decoded arguments invalid JSON: %v", err)
		}
		if args["text"] != `keep, } and ", ]` {
			t.Fatalf("string comma modified: %#v", args["text"])
		}
	})

	t.Run("genuinely malformed JSON still fails", func(t *testing.T) {
		if events, ok := assistantCalls(`[{"name": "echo", "arguments": {bad}]`, true, []string{"echo"}); ok || len(events) != 0 {
			t.Fatalf("expected failure for invalid JSON, got ok=%v events=%#v", ok, events)
		}
	})
}

func TestAssistantStreamDecoderInvalidToolDetection(t *testing.T) {
	t.Run("forbidden workspace tool sets invalidTool and stops bogus steps", func(t *testing.T) {
		decoder := &AssistantStreamDecoder{Functions: []string{"read_file"}}
		payload := assistantStreamPayload(t,
			assistantMessage("applet-1",
				assistantModelStep(1, 1, "Checking file.\n", nil),
				assistantModelStep(2, 1, "", assistantActionTool([2]string{"tag", "run_command"}, [2]string{"command", "ls"})),
				assistantModelStep(3, 1, "BOGUS WORKSPACE OUTPUT", nil),
			),
		)
		var events []Event
		if err := DecodeAssistantStream(strings.NewReader(payload), decoder, func(event Event) error {
			events = append(events, event)
			return nil
		}); err != nil {
			t.Fatalf("DecodeAssistantStream failed: %v", err)
		}
		if !decoder.invalidTool || !decoder.overran || decoder.called {
			t.Fatalf("unexpected decoder flags: invalidTool=%v overran=%v called=%v", decoder.invalidTool, decoder.overran, decoder.called)
		}
		var text strings.Builder
		var finish string
		for _, event := range events {
			if event.Kind == EventText {
				text.WriteString(event.Text)
			}
			if event.Kind == EventFinish {
				finish = event.FinishReason
			}
		}
		if text.String() != "Checking file.\n" {
			t.Fatalf("unexpected text (bogus step should be skipped): %q", text.String())
		}
		if finish != decodeFinishReason(10) {
			t.Fatalf("finish reason=%q, want %q", finish, decodeFinishReason(10))
		}
	})

	t.Run("search_web and user-visible ask_question do not set invalidTool", func(t *testing.T) {
		questions, _ := json.Marshal([]map[string]any{{"question": "Which environment?", "options": []string{"dev", "prod"}}})
		decoder := &AssistantStreamDecoder{Functions: []string{"read_file"}}
		payload := assistantStreamPayload(t,
			assistantMessage("applet-1",
				assistantModelStep(1, 1, "", assistantActionSlot(18, []any{"go test", nil, "summary", []any{[]any{"Go", "https://go.dev"}}})),
				assistantModelStep(2, 1, "", assistantActionTool([2]string{"tag", assistantQuestionTool}, [2]string{"questions", string(questions)})),
			),
		)
		var events []Event
		if err := DecodeAssistantStream(strings.NewReader(payload), decoder, func(event Event) error {
			events = append(events, event)
			return nil
		}); err != nil {
			t.Fatalf("DecodeAssistantStream failed: %v", err)
		}
		if decoder.invalidTool {
			t.Fatalf("expected invalidTool=false for search_web + user question")
		}
		var gotText, gotGrounding bool
		var finish string
		for _, event := range events {
			switch event.Kind {
			case EventText:
				gotText = true
			case EventGrounding:
				gotGrounding = true
			case EventFinish:
				finish = event.FinishReason
			}
		}
		if !gotText || !gotGrounding || finish != "stop" {
			t.Fatalf("gotText=%v gotGrounding=%v finish=%q", gotText, gotGrounding, finish)
		}
	})

	t.Run("same-step function_calls and ask_question call question does not set invalidTool", func(t *testing.T) {
		questions, _ := json.Marshal([]map[string]any{{"question": assistantCallQuestion}})
		decoder := &AssistantStreamDecoder{Functions: []string{"read_file"}}
		callText := "```function_calls\n[{\"name\":\"read_file\",\"arguments\":{\"path\":\"a.txt\"}}]\n```"
		payload := assistantStreamPayload(t,
			assistantMessage("applet-1",
				assistantModelStep(1, 1, callText, assistantActionTool([2]string{"tag", assistantQuestionTool}, [2]string{"questions", string(questions)})),
			),
		)
		var events []Event
		if err := DecodeAssistantStream(strings.NewReader(payload), decoder, func(event Event) error {
			events = append(events, event)
			return nil
		}); err != nil {
			t.Fatalf("DecodeAssistantStream failed: %v", err)
		}
		if decoder.invalidTool || !decoder.called {
			t.Fatalf("expected called=true and invalidTool=false, got called=%v invalidTool=%v", decoder.called, decoder.invalidTool)
		}
		var finish string
		for _, event := range events {
			if event.Kind == EventFinish {
				finish = event.FinishReason
			}
		}
		if finish != "stop" {
			t.Fatalf("finish reason=%q, want stop", finish)
		}
	})
}

func TestSendAssistantRecoversMalformedFunctionCallWithinSameApplet(t *testing.T) {
	transport := &fakeAssistantTransport{
		streams: []string{
			assistantStreamPayload(t,
				assistantMessage("applet-1",
					assistantModelStep(1, 1, "Let me read that file.\n```function_calls\n[{\"name\": \"read_file\", \"arguments\": {broken}]\n```\n", nil),
				),
			),
			assistantStreamPayload(t,
				assistantMessage("applet-1",
					assistantModelStep(1, 2, "Let me read ", nil),
				),
				assistantMessage("applet-1",
					assistantModelStep(1, 1, "Let me read that file.\n```function_calls\n[{\"name\": \"read_file\", \"arguments\": {\"path\": \"config.yaml\"}}]\n```\n", nil),
				),
			),
		},
	}

	events, body := runSendAssistantTest(t, transport, []FunctionDeclaration{{Name: "read_file"}})
	defer body.Close()

	if transport.createCount() != 1 {
		t.Fatalf("CreateApplet calls=%d, want 1", transport.createCount())
	}
	calls := transport.offlineSnapshot()
	if len(calls) != 2 {
		t.Fatalf("CodeAssistantOffline calls=%d, want 2", len(calls))
	}
	if calls[0].continued || !calls[1].continued || calls[0].applet != calls[1].applet {
		t.Fatalf("unexpected continuation calls: %+v", calls)
	}
	followUpPrompt := calls[1].contents[0].Parts[0].Text
	if !strings.Contains(followUpPrompt, assistantMalformedCallPrompt) {
		t.Fatalf("follow-up prompt missing assistantMalformedCallPrompt: %q", followUpPrompt)
	}
	if !strings.Contains(followUpPrompt, "<partial_reply>\nLet me read that file.\n\n</partial_reply>") {
		t.Fatalf("follow-up prompt missing partial reply tail: %q", followUpPrompt)
	}

	assertRecoveredToolCallEvents(t, events, "Let me read that file.\n", "read_file", `{"path":"config.yaml"}`)
}

func TestSendAssistantRecoversMalformedFunctionCallWithoutPriorText(t *testing.T) {
	transport := &fakeAssistantTransport{
		streams: []string{
			assistantStreamPayload(t,
				assistantMessage("applet-1",
					assistantModelStep(1, 1, "```function_calls\n[{\"name\": \"read_file\", \"arguments\": {broken}]\n```\n", nil),
				),
			),
			assistantStreamPayload(t,
				assistantMessage("applet-1",
					assistantModelStep(1, 1, "```function_calls\n[{\"name\": \"read_file\", \"arguments\": {\"path\": \"config.yaml\"}}]\n```\n", nil),
				),
			),
		},
	}

	events, body := runSendAssistantTest(t, transport, []FunctionDeclaration{{Name: "read_file"}})
	defer body.Close()

	calls := transport.offlineSnapshot()
	if len(calls) != 2 {
		t.Fatalf("CodeAssistantOffline calls=%d, want 2", len(calls))
	}
	followUpPrompt := calls[1].contents[0].Parts[0].Text
	if followUpPrompt != assistantMalformedCallPrompt {
		t.Fatalf("expected exact assistantMalformedCallPrompt without partial_reply, got %q", followUpPrompt)
	}
	assertRecoveredToolCallEvents(t, events, "", "read_file", `{"path":"config.yaml"}`)
}

func TestSendAssistantRecoversForbiddenWorkspaceToolWithinSameApplet(t *testing.T) {
	for _, tc := range []struct {
		name   string
		action []any
	}{
		{
			name:   "run_command in action[21]",
			action: assistantActionTool([2]string{"tag", "run_command"}, [2]string{"command", "pwd"}),
		},
		{
			name:   "view_file in action[0]",
			action: assistantActionSlot(0, []any{"view_file", "main.go"}),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			transport := &fakeAssistantTransport{
				streams: []string{
					assistantStreamPayload(t,
						assistantMessage("applet-1",
							assistantModelStep(1, 1, "Checking workspace now.\n", nil),
							assistantModelStep(2, 1, "", tc.action),
							assistantModelStep(3, 1, "BOGUS WORKSPACE RESULT", nil),
						),
					),
					assistantStreamPayload(t,
						assistantMessage("applet-1",
							assistantModelStep(1, 2, "Checking ", nil),
						),
						assistantMessage("applet-1",
							assistantModelStep(1, 1, "Checking workspace now.\nNow calling client tool.\n```function_calls\n[{\"name\": \"run_shell\", \"arguments\": {\"cmd\": \"pwd\"}}]\n```\n", nil),
						),
					),
				},
			}

			events, body := runSendAssistantTest(t, transport, []FunctionDeclaration{{Name: "run_shell"}})
			defer body.Close()

			if transport.createCount() != 1 {
				t.Fatalf("CreateApplet calls=%d, want 1", transport.createCount())
			}
			calls := transport.offlineSnapshot()
			if len(calls) != 2 {
				t.Fatalf("CodeAssistantOffline calls=%d, want 2", len(calls))
			}
			if calls[0].continued || !calls[1].continued || calls[0].applet != calls[1].applet {
				t.Fatalf("unexpected continuation calls: %+v", calls)
			}
			followUpPrompt := calls[1].contents[0].Parts[0].Text
			if !strings.Contains(followUpPrompt, assistantInvalidToolPrompt) {
				t.Fatalf("follow-up prompt missing assistantInvalidToolPrompt: %q", followUpPrompt)
			}
			if !strings.Contains(followUpPrompt, "<partial_reply>\nChecking workspace now.\n\n</partial_reply>") {
				t.Fatalf("follow-up prompt missing partial reply tail: %q", followUpPrompt)
			}

			assertRecoveredToolCallEvents(t, events, "Checking workspace now.\nNow calling client tool.\n", "run_shell", `{"cmd":"pwd"}`)
		})
	}
}

func TestSendAssistantExhaustedInvalidToolEndsWithMalformedFinish(t *testing.T) {
	badTurn := assistantStreamPayload(t,
		assistantMessage("applet-1",
			assistantModelStep(1, 1, "Trying workspace tool.\n", nil),
			assistantModelStep(2, 1, "", assistantActionTool([2]string{"tag", "run_command"}, [2]string{"command", "ls"})),
		),
	)
	streams := make([]string, regenerations+1)
	for index := range streams {
		streams[index] = badTurn
	}
	transport := &fakeAssistantTransport{streams: streams}

	events, body := runSendAssistantTest(t, transport, []FunctionDeclaration{{Name: "run_shell"}})
	defer body.Close()

	calls := transport.offlineSnapshot()
	if len(calls) != regenerations+1 {
		t.Fatalf("CodeAssistantOffline calls=%d, want %d", len(calls), regenerations+1)
	}
	var text strings.Builder
	var finishes []Event
	for _, event := range events {
		switch event.Kind {
		case EventText:
			text.WriteString(event.Text)
		case EventFinish:
			finishes = append(finishes, event)
		}
	}
	if text.String() != "Trying workspace tool.\n" {
		t.Fatalf("expected duplicate prefix stripped across retries, got %q", text.String())
	}
	if len(finishes) != 1 || finishes[0].FinishReason != decodeFinishReason(10) {
		t.Fatalf("expected 1 final malformed_function_call finish, got %+v", finishes)
	}
}

func runSendAssistantTest(t *testing.T, transport *fakeAssistantTransport, functions []FunctionDeclaration) ([]Event, io.ReadCloser) {
	t.Helper()
	baseModel := Model{
		ID:           "gemini-2.5-pro",
		Name:         "Gemini 2.5 Pro",
		Methods:      []string{"generateContent"},
		Capabilities: map[string]bool{"chat_model": true, "code_assistant": true},
	}
	entry := assistantModelEntry(modelEntry{
		model: baseModel,
		defaults: GenerationDefaults{
			MaxOutputTokens: 65536,
			Thinking:        true,
			ThinkingLevel:   true,
			ThinkingLevels:  []int64{1, 2, 3, 4},
		},
	})
	account := &Account{
		ID:                    "test@example.com",
		Config:                AccountConfig{Enabled: true},
		State:                 AccountReady,
		Models:                []Model{baseModel, entry.model},
		modelAccessGeneration: 1,
	}
	pool := NewAccountPool([]*Account{account}, 1)
	lease, _, err := pool.TryAcquireFor(context.Background(), AccountSelection{ModelID: entry.model.ID, Method: "generateContent"})
	if err != nil || lease == nil {
		t.Fatalf("TryAcquireFor failed: lease=%v err=%v", lease, err)
	}
	t.Cleanup(func() {
		_ = lease.Release()
	})
	ctx := ContextWithAccountLease(context.Background(), lease)
	client := NewClient(ClientOptions{Transport: transport, Protected: transport})
	request := GenerateRequest{
		ID:        "req-1",
		Model:     entry.model.ID,
		AccountID: account.ID,
		Contents:  []Content{{Role: RoleUser, Parts: []Part{{Text: "Perform the task"}}}},
		Tools:     Tools{Functions: functions},
	}
	response, decodeStream, err := client.sendAssistant(ctx, request, entry)
	if err != nil {
		t.Fatalf("sendAssistant failed: %v", err)
	}
	var events []Event
	if err := decodeStream(response.Body, func(event Event) error {
		events = append(events, event)
		return nil
	}); err != nil {
		_ = response.Body.Close()
		t.Fatalf("decodeStream failed: %v", err)
	}
	return events, response.Body
}

func assertRecoveredToolCallEvents(t *testing.T, events []Event, wantText string, wantTool string, wantArgs string) {
	t.Helper()
	var text strings.Builder
	var toolCalls []*FunctionCall
	var finishes []Event
	for _, event := range events {
		switch event.Kind {
		case EventText:
			text.WriteString(event.Text)
		case EventToolCall:
			toolCalls = append(toolCalls, event.FunctionCall)
		case EventFinish:
			finishes = append(finishes, event)
		}
	}
	if text.String() != wantText {
		t.Fatalf("text=%q, want %q", text.String(), wantText)
	}
	if len(toolCalls) != 1 || toolCalls[0] == nil || toolCalls[0].Name != wantTool {
		t.Fatalf("unexpected toolCalls: %+v", toolCalls)
	}
	var gotArgs, wantCompact bytes.Buffer
	if err := json.Compact(&gotArgs, toolCalls[0].Arguments); err != nil {
		t.Fatalf("invalid tool arguments JSON %q: %v", string(toolCalls[0].Arguments), err)
	}
	if err := json.Compact(&wantCompact, []byte(wantArgs)); err != nil {
		t.Fatalf("invalid wantArgs JSON %q: %v", wantArgs, err)
	}
	if !bytes.Equal(gotArgs.Bytes(), wantCompact.Bytes()) {
		t.Fatalf("tool arguments=%s, want %s", gotArgs.String(), wantCompact.String())
	}
	if len(finishes) != 1 || finishes[0].FinishReason != "stop" {
		t.Fatalf("expected single stop finish event, got %+v", finishes)
	}
}

type recordedAssistantCall struct {
	contents  []Content
	continued bool
	applet    string
}

type fakeAssistantTransport struct {
	mu           sync.Mutex
	createCalls  int
	cancelCalls  int
	offlineCalls []recordedAssistantCall
	streams      []string
	streamIndex  int
}

func (f *fakeAssistantTransport) createCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.createCalls
}

func (f *fakeAssistantTransport) offlineSnapshot() []recordedAssistantCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]recordedAssistantCall, len(f.offlineCalls))
	copy(out, f.offlineCalls)
	return out
}

func (f *fakeAssistantTransport) Do(_ context.Context, rpc RPCRequest) (*RPCResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var body string
	switch rpc.Method {
	case "CreateApplet":
		f.createCalls++
		body = `["applet-1"]`
	case assistantStreamMethod:
		if f.streamIndex >= len(f.streams) {
			body = `[[],[0]]`
		} else {
			body = f.streams[f.streamIndex]
			f.streamIndex++
		}
	case "CancelCodeAssistantOfflineGeneration":
		f.cancelCalls++
		body = `[]`
	default:
		body = `[]`
	}
	return &RPCResponse{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{JSONProtobufContentType}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}, nil
}

func (f *fakeAssistantTransport) DoProtected(_ context.Context, request GenerateRequest, rpc RPCRequest) (*RPCResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var wire []json.RawMessage
	if err := json.Unmarshal(rpc.Body, &wire); err != nil {
		return nil, err
	}
	var mode int64
	_ = json.Unmarshal(wire[5], &mode)
	var applet string
	_ = json.Unmarshal(wire[11], &applet)
	f.offlineCalls = append(f.offlineCalls, recordedAssistantCall{
		contents:  request.Contents,
		continued: mode == 2,
		applet:    applet,
	})
	return &RPCResponse{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{JSONProtobufContentType}},
		Body:       io.NopCloser(strings.NewReader(fmt.Sprintf(`["gen-%d",%q]`, len(f.offlineCalls), applet))),
	}, nil
}

func assistantStreamPayload(t *testing.T, messages ...[]any) string {
	t.Helper()
	encoded, err := json.Marshal([]any{messages, []any{0}})
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

func assistantMessage(applet string, steps ...[]any) []any {
	return []any{nil, nil, steps, applet}
}

func assistantModelStep(number int64, status int64, text string, action []any) []any {
	step := make([]any, 16)
	if text != "" {
		step[0] = []any{[]any{[]any{[]any{nil, text}}, "model"}}
	}
	step[9] = action
	step[11] = number
	step[12] = status
	return step
}

func assistantActionTool(pairs ...[2]string) []any {
	action := make([]any, 22)
	wirePairs := make([][]string, 0, len(pairs))
	for _, pair := range pairs {
		wirePairs = append(wirePairs, []string{pair[0], pair[1]})
	}
	action[21] = []any{"Tool", nil, nil, wirePairs}
	return action
}

func assistantActionSlot(index int, value any) []any {
	action := make([]any, index+1)
	action[index] = value
	return action
}
