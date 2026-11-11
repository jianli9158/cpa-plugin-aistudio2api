package aistudio

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

type fakeRecoveryProtectedTransport struct {
	methods   []string
	bodies    []string
	responses []string
}

func (t *fakeRecoveryProtectedTransport) DoProtected(_ context.Context, _ GenerateRequest, rpc RPCRequest) (*RPCResponse, error) {
	index := len(t.bodies)
	t.methods = append(t.methods, rpc.Method)
	t.bodies = append(t.bodies, string(rpc.Body))
	body := ""
	if index < len(t.responses) {
		body = t.responses[index]
	}
	return &RPCResponse{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{JSONProtobufContentType}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}, nil
}

func newRecoveryTestClient(modelID string, responses ...string) (*Client, *fakeRecoveryProtectedTransport) {
	transport := &fakeRecoveryProtectedTransport{responses: responses}
	client := NewClient(ClientOptions{Protected: transport})
	model := Model{
		ID:               modelID,
		Name:             "Gemini Recovery Test",
		Methods:          []string{"generateContent"},
		OutputTokenLimit: 8192,
		Capabilities: map[string]bool{
			"chat_model":            true,
			"function_declarations": true,
			"thinking":              true,
		},
	}
	client.setModelCatalog("acc@example.com", modelCatalog{
		models: []Model{model},
		entries: map[string]modelEntry{
			modelID: {
				model:    model,
				defaults: GenerationDefaults{MaxOutputTokens: 8192, Thinking: true},
			},
		},
	})
	return client, transport
}

func playgroundTextWirePart(text string) []any {
	return []any{nil, text}
}

func playgroundThoughtWirePart(text string) []any {
	part := make([]any, 13)
	part[1] = text
	part[12] = int64(1)
	return part
}

func playgroundSignatureWirePart(sig string) []any {
	part := make([]any, 15)
	part[1] = ""
	part[14] = sig
	return part
}

func playgroundCallWirePart(id string, name string, args map[string]any) []any {
	part := make([]any, 11)
	part[10] = []any{name, encodeWireStruct(args), id}
	return part
}

func playgroundStreamFrame(parts [][]any, finishCode *int64, finishMessage string, usage *Usage) []any {
	candidate := []any{[]any{parts, "model"}}
	if finishCode != nil {
		candidate = append(candidate, *finishCode)
		if finishMessage != "" {
			candidate = append(candidate, nil, finishMessage)
		}
	}
	frame := make([]any, 3)
	frame[0] = []any{candidate}
	if usage != nil {
		frame[2] = []any{usage.InputTokens, usage.OutputTokens, usage.TotalTokens}
	}
	return frame
}

func playgroundStreamBody(t *testing.T, frames ...[]any) string {
	t.Helper()
	raw, err := json.Marshal([]any{frames, []any{int64(0)}})
	if err != nil {
		t.Fatalf("编码 Playground 流失败: %v", err)
	}
	return string(raw)
}

func buildStreamBody(t *testing.T, chunks ...string) string {
	t.Helper()
	wrapped := make([]any, 0, len(chunks))
	for _, chunk := range chunks {
		wrapped = append(wrapped, []any{chunk})
	}
	raw, err := json.Marshal([]any{wrapped, []any{int64(0)}})
	if err != nil {
		t.Fatalf("编码 Build 流失败: %v", err)
	}
	return string(raw)
}

func collectRecoveryEvents(ch <-chan Event) []Event {
	var collected []Event
	for event := range ch {
		collected = append(collected, event)
	}
	return collected
}

// TestAbnormalGenerationFinish 验证异常终止原因集合的精确匹配
func TestAbnormalGenerationFinish(t *testing.T) {
	for _, item := range []struct {
		reason string
		want   bool
	}{
		{"malformed_function_call", true},
		{"unexpected_tool_call", true},
		{"malformed_response", true},
		{"stop", false},
		{"max_tokens", false},
		{"safety", false},
		{"", false},
	} {
		if got := abnormalGenerationFinish(item.reason); got != item.want {
			t.Errorf("abnormalGenerationFinish(%q) = %v, want %v", item.reason, got, item.want)
		}
	}
}

// TestCommittedTextOnly 验证纯文本判定会忽略思考与纯签名并拒绝工具调用/媒体等非文本输出
func TestCommittedTextOnly(t *testing.T) {
	t.Run("empty", func(t *testing.T) {
		if text, ok := committedTextOnly(Output{}); ok || text != "" {
			t.Fatalf("committedTextOnly(empty) = (%q, %v)", text, ok)
		}
	})

	t.Run("thought and signature only", func(t *testing.T) {
		out := Output{Parts: []Part{
			{Text: "hidden thinking", Thought: true},
			{ThoughtSignature: "sig-1"},
		}}
		if text, ok := committedTextOnly(out); ok || text != "" {
			t.Fatalf("committedTextOnly(thought+sig) = (%q, %v)", text, ok)
		}
	})

	t.Run("plain text with signature", func(t *testing.T) {
		out := Output{Parts: []Part{
			{Text: "hidden", Thought: true},
			{Text: "Hello "},
			{Text: "world", ThoughtSignature: "sig-text"},
			{ThoughtSignature: "sig-tail"},
		}}
		text, ok := committedTextOnly(out)
		if !ok || text != "Hello world" {
			t.Fatalf("committedTextOnly(text) = (%q, %v)", text, ok)
		}
	})

	t.Run("rejects non-text payloads", func(t *testing.T) {
		for name, part := range map[string]Part{
			"function_call": {FunctionCall: &FunctionCall{Name: "get_weather", Arguments: json.RawMessage(`{}`)}},
			"media":         {InlineData: &Blob{MIME: "image/png", Data: []byte{1}}},
			"exec_code":     {ExecutableCode: &ExecutableCode{Language: "PYTHON", Code: "print(1)"}},
			"server_tool":   {ToolCall: &ServerToolStep{Type: "GOOGLE_SEARCH"}},
			"transcript":    {Text: "hi", Transcript: &TranscriptMetadata{Text: "hi"}},
		} {
			out := Output{Parts: []Part{{Text: "prefix"}, part}}
			if text, ok := committedTextOnly(out); ok || text != "" {
				t.Errorf("%s: committedTextOnly = (%q, %v), want (\"\", false)", name, text, ok)
			}
		}
	})
}

// TestContinuationPrefixStripper 验证续写前缀去重在分块重复、超长尾部回显、签名保留与分叉回退下的行为
func TestContinuationPrefixStripper(t *testing.T) {
	t.Run("multi-chunk prefix stripped", func(t *testing.T) {
		stripper := newContinuationPrefixStripper("Hello world")
		var emitted []Event
		emit := func(event Event) error {
			emitted = append(emitted, event)
			return nil
		}
		for _, event := range []Event{
			{Kind: EventUsage, Usage: &Usage{InputTokens: 1}},
			{Kind: EventReasoning, Part: Part{Text: "thinking", Thought: true}},
			{Kind: EventText, Part: Part{Text: "Hello "}},
			{Kind: EventText, Part: Part{Text: "world! Next", ThoughtSignature: "sig-1"}},
			{Kind: EventText, Part: Part{Text: " chunk"}},
		} {
			if err := stripper.filter(event, emit); err != nil {
				t.Fatal(err)
			}
		}
		if len(emitted) != 4 {
			t.Fatalf("emitted len = %d, want 4: %+v", len(emitted), emitted)
		}
		if emitted[2].Kind != EventText || emitted[2].Text != "! Next" || emitted[2].ThoughtSignature != "sig-1" {
			t.Fatalf("emitted[2] = %+v", emitted[2])
		}
		if emitted[3].Text != " chunk" {
			t.Fatalf("emitted[3] = %+v", emitted[3])
		}
	})

	t.Run("matched chunk with thought signature emits signature", func(t *testing.T) {
		stripper := newContinuationPrefixStripper("Alpha")
		var emitted []Event
		emit := func(event Event) error {
			emitted = append(emitted, event)
			return nil
		}
		if err := stripper.filter(Event{Kind: EventText, Part: Part{Text: "Alpha", ThoughtSignature: "sig-dup"}}, emit); err != nil {
			t.Fatal(err)
		}
		if len(emitted) != 1 || emitted[0].Kind != EventThoughtSignature || emitted[0].ThoughtSignature != "sig-dup" {
			t.Fatalf("emitted = %+v", emitted)
		}
	})

	t.Run("long reply matches tail when prefix clipped", func(t *testing.T) {
		prefix := "HEAD-"
		tail := strings.Repeat("a", assistantPartialReplyRunes)
		full := prefix + tail
		stripper := newContinuationPrefixStripper(full)
		var emitted []Event
		emit := func(event Event) error {
			emitted = append(emitted, event)
			return nil
		}
		if err := stripper.filter(Event{Kind: EventText, Part: Part{Text: tail + " continued"}}, emit); err != nil {
			t.Fatal(err)
		}
		if len(emitted) != 1 || emitted[0].Text != " continued" {
			t.Fatalf("emitted = %+v", emitted)
		}
	})

	t.Run("divergence flushes pending text", func(t *testing.T) {
		stripper := newContinuationPrefixStripper("Hello world")
		var emitted []Event
		emit := func(event Event) error {
			emitted = append(emitted, event)
			return nil
		}
		if err := stripper.filter(Event{Kind: EventText, Part: Part{Text: "Hello "}}, emit); err != nil {
			t.Fatal(err)
		}
		if len(emitted) != 0 {
			t.Fatalf("expected pending buffer, got %+v", emitted)
		}
		if err := stripper.filter(Event{Kind: EventText, Part: Part{Text: "there"}}, emit); err != nil {
			t.Fatal(err)
		}
		if len(emitted) != 1 || emitted[0].Text != "Hello there" {
			t.Fatalf("emitted = %+v", emitted)
		}
	})
}

// TestGenerateMidStreamRecoveryAfterMalformedFunctionCall 验证 Playground 流在提交纯文本后遇到 malformed_function_call 时同流续写恢复工具调用与用量
func TestGenerateMidStreamRecoveryAfterMalformedFunctionCall(t *testing.T) {
	malformedCode := int64(10)
	stopCode := int64(1)

	attempt1 := playgroundStreamBody(t,
		playgroundStreamFrame([][]any{playgroundTextWirePart("Let me check the weather. ")}, nil, "", nil),
		playgroundStreamFrame([][]any{
			playgroundTextWirePart("Hold on"),
			playgroundSignatureWirePart("sig-failed"),
		}, &malformedCode, "", &Usage{InputTokens: 10, OutputTokens: 5, TotalTokens: 15}),
	)
	attempt2 := playgroundStreamBody(t,
		playgroundStreamFrame([][]any{playgroundTextWirePart("Let me check the weather. ")}, nil, "", nil),
		playgroundStreamFrame([][]any{playgroundTextWirePart("Hold on, calling tool now.")}, nil, "", nil),
		playgroundStreamFrame([][]any{
			playgroundCallWirePart("call_1", "get_weather", map[string]any{"city": "Tokyo"}),
		}, &stopCode, "", &Usage{InputTokens: 25, OutputTokens: 8, TotalTokens: 33}),
	)

	client, transport := newRecoveryTestClient("gemini-test", attempt1, attempt2)
	request := GenerateRequest{
		ID:        "req-recover-1",
		AccountID: "acc@example.com",
		Model:     "gemini-test",
		Contents: []Content{
			{Role: RoleUser, Parts: []Part{{Text: "What is the weather in Tokyo?"}}},
		},
		Tools: Tools{
			Functions: []FunctionDeclaration{{
				Name:       "get_weather",
				Parameters: json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}`),
			}},
			ToolConfig: ToolConfig{Mode: "required"},
		},
	}

	stream, err := client.Generate(context.Background(), request)
	if err != nil {
		t.Fatalf("Generate failed: %v", err)
	}
	events := collectRecoveryEvents(stream)

	if len(transport.bodies) != 2 {
		t.Fatalf("request attempts = %d, want 2", len(transport.bodies))
	}

	// 验证第 2 次请求的 wire contents 末尾补入了已输出文本与续写提示
	var wire []json.RawMessage
	if err := json.Unmarshal([]byte(transport.bodies[1]), &wire); err != nil {
		t.Fatalf("unmarshal retry wire body: %v", err)
	}
	var wireContents [][]json.RawMessage
	if err := json.Unmarshal(wire[1], &wireContents); err != nil {
		t.Fatalf("unmarshal retry contents: %v", err)
	}
	if len(wireContents) != 3 {
		t.Fatalf("retry contents len = %d, want 3", len(wireContents))
	}
	decodeContentTurn := func(raw []json.RawMessage) (string, string) {
		var role string
		_ = json.Unmarshal(raw[1], &role)
		var parts [][]json.RawMessage
		_ = json.Unmarshal(raw[0], &parts)
		var text string
		if len(parts) > 0 && len(parts[0]) > 1 {
			_ = json.Unmarshal(parts[0][1], &text)
		}
		return role, text
	}
	if role, text := decodeContentTurn(wireContents[1]); role != "model" || text != "Let me check the weather. Hold on" {
		t.Fatalf("assistant partial content = (%q, %q)", role, text)
	}
	if role, text := decodeContentTurn(wireContents[2]); role != "user" || !strings.Contains(text, "<partial_reply>\nLet me check the weather. Hold on\n</partial_reply>") {
		t.Fatalf("continuation prompt content = (%q, %q)", role, text)
	}

	var textOut strings.Builder
	var toolCalls []FunctionCall
	var usage *Usage
	var finish *Event
	for _, event := range events {
		switch event.Kind {
		case EventText:
			textOut.WriteString(event.Text)
		case EventToolCall:
			if event.FunctionCall != nil {
				toolCalls = append(toolCalls, *event.FunctionCall)
			}
		case EventUsage:
			u := *event.Usage
			usage = &u
		case EventFinish:
			f := event
			finish = &f
		case EventError:
			t.Fatalf("unexpected EventError: %v", event.Err)
		}
	}

	if textOut.String() != "Let me check the weather. Hold on, calling tool now." {
		t.Fatalf("streamed text = %q", textOut.String())
	}
	if len(toolCalls) != 1 || toolCalls[0].Name != "get_weather" {
		t.Fatalf("toolCalls = %+v", toolCalls)
	}
	var args map[string]string
	if err := json.Unmarshal(toolCalls[0].Arguments, &args); err != nil || !reflect.DeepEqual(args, map[string]string{"city": "Tokyo"}) {
		t.Fatalf("tool args = %s, err = %v", toolCalls[0].Arguments, err)
	}
	if usage == nil || usage.InputTokens != 24 || usage.OutputTokens != 13 || usage.TotalTokens != 37 {
		t.Fatalf("merged usage = %+v", usage)
	}
	if finish == nil || finish.FinishReason != "stop" {
		t.Fatalf("finish = %+v", finish)
	}
}

// TestGenerateMidStreamRecoveryExhaustedEmitsAbnormalFinishError 验证恢复次数耗尽仍异常终止时发送 EventError(*AbnormalFinishError) 而非 EventFinish
func TestGenerateMidStreamRecoveryExhaustedEmitsAbnormalFinishError(t *testing.T) {
	malformedCode := int64(10)
	failedFrame := playgroundStreamBody(t,
		playgroundStreamFrame([][]any{playgroundTextWirePart("Partial reply.")}, &malformedCode, "bad function args", &Usage{InputTokens: 10, OutputTokens: 3, TotalTokens: 13}),
	)

	responses := make([]string, regenerations+1)
	for index := range responses {
		responses[index] = failedFrame
	}
	client, transport := newRecoveryTestClient("gemini-test", responses...)

	stream, err := client.Generate(context.Background(), GenerateRequest{
		ID:        "req-exhausted",
		AccountID: "acc@example.com",
		Model:     "gemini-test",
		Contents:  []Content{{Role: RoleUser, Parts: []Part{{Text: "Call tool"}}}},
		Tools: Tools{
			Functions:  []FunctionDeclaration{{Name: "ping", Parameters: json.RawMessage(`{"type":"object"}`)}},
			ToolConfig: ToolConfig{Mode: "required"},
		},
	})
	if err != nil {
		t.Fatalf("Generate failed: %v", err)
	}
	events := collectRecoveryEvents(stream)

	if len(transport.bodies) != regenerations+1 {
		t.Fatalf("attempts = %d, want %d", len(transport.bodies), regenerations+1)
	}

	var sawUsage bool
	var sawFinish bool
	var abnormal *AbnormalFinishError
	for _, event := range events {
		switch event.Kind {
		case EventUsage:
			sawUsage = true
		case EventFinish:
			sawFinish = true
		case EventError:
			if !errors.As(event.Err, &abnormal) {
				t.Fatalf("expected *AbnormalFinishError, got %T (%v)", event.Err, event.Err)
			}
		}
	}
	if !sawUsage {
		t.Fatalf("expected EventUsage before EventError")
	}
	if sawFinish {
		t.Fatalf("did not expect EventFinish when retries exhausted with malformed_function_call")
	}
	if abnormal == nil || abnormal.Reason != "malformed_function_call" || abnormal.Message != "bad function args" {
		t.Fatalf("abnormal = %+v", abnormal)
	}
}

// TestGenerateBuildMidStreamRecovery 验证 Build 通道流在提交纯文本后遇到 MALFORMED_FUNCTION_CALL 时同流续写恢复
func TestGenerateBuildMidStreamRecovery(t *testing.T) {
	chunk1 := `{"candidates":[{"content":{"parts":[{"text":"Checking build..."}]},"finishReason":"MALFORMED_FUNCTION_CALL"}],"usageMetadata":{"promptTokenCount":12,"candidatesTokenCount":4,"totalTokenCount":16}}`
	chunk2 := `{"candidates":[{"content":{"parts":[{"text":"Checking build... done."},{"functionCall":{"id":"c1","name":"get_weather","args":{"city":"Osaka"}}}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":30,"candidatesTokenCount":6,"totalTokenCount":36}}`

	client, transport := newRecoveryTestClient("gemini-build-test",
		buildStreamBody(t, chunk1),
		buildStreamBody(t, chunk2),
	)
	model := Model{
		ID:               "gemini-build-test",
		Name:             "Gemini Build Test",
		Methods:          []string{"generateContent"},
		OutputTokenLimit: 8192,
		Capabilities:     map[string]bool{"chat_model": true, "function_declarations": true},
	}
	account := &Account{ID: "acc@example.com", Config: AccountConfig{Enabled: true}, State: AccountReady, Models: []Model{model}, modelAccessGeneration: 1}
	pool := NewAccountPool([]*Account{account}, 1)
	pool.SetUpstreamChannels([]Channel{ChannelBuild})
	if err := pool.SetBuildCatalog(account.ID, []Model{model}); err != nil {
		t.Fatal(err)
	}
	lease, ok, err := pool.TryAcquireFor(context.Background(), AccountSelection{ModelID: model.ID, Method: "generateContent", Channel: ChannelBuild})
	if err != nil || !ok {
		t.Fatalf("TryAcquireFor ok=%v err=%v", ok, err)
	}
	defer func() { _ = lease.Release() }()

	ctx := ContextWithAccountLease(context.Background(), lease)
	stream, err := client.Generate(ctx, GenerateRequest{
		ID:        "req-build-recover",
		AccountID: account.ID,
		Model:     model.ID,
		Contents:  []Content{{Role: RoleUser, Parts: []Part{{Text: "Check Osaka weather"}}}},
		Tools: Tools{
			Functions: []FunctionDeclaration{{
				Name:       "get_weather",
				Parameters: json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}}}`),
			}},
		},
	})
	if err != nil {
		t.Fatalf("Generate failed: %v", err)
	}
	events := collectRecoveryEvents(stream)

	if len(transport.bodies) != 2 || transport.methods[0] != buildProxyStreamedMethod || transport.methods[1] != buildProxyStreamedMethod {
		t.Fatalf("methods = %v", transport.methods)
	}
	var proxyWire []json.RawMessage
	if err := json.Unmarshal([]byte(transport.bodies[1]), &proxyWire); err != nil || len(proxyWire) < 2 {
		t.Fatalf("unmarshal build proxy request: %v (%v)", err, transport.bodies[1])
	}
	var buildBodyText string
	if err := json.Unmarshal(proxyWire[1], &buildBodyText); err != nil {
		t.Fatalf("unmarshal build request string: %v", err)
	}
	var buildBody struct {
		Contents []struct {
			Role  string `json:"role"`
			Parts []struct {
				Text string `json:"text"`
			} `json:"parts"`
		} `json:"contents"`
	}
	if err := json.Unmarshal([]byte(buildBodyText), &buildBody); err != nil {
		t.Fatalf("unmarshal build contents: %v", err)
	}
	if len(buildBody.Contents) != 3 || buildBody.Contents[1].Role != "model" || buildBody.Contents[1].Parts[0].Text != "Checking build..." ||
		buildBody.Contents[2].Role != "user" || !strings.Contains(buildBody.Contents[2].Parts[0].Text, "<partial_reply>\nChecking build...\n</partial_reply>") {
		t.Fatalf("build retry contents = %+v", buildBody.Contents)
	}

	var textOut strings.Builder
	var toolCall *FunctionCall
	var finish *Event
	for _, event := range events {
		switch event.Kind {
		case EventText:
			textOut.WriteString(event.Text)
		case EventToolCall:
			call := *event.FunctionCall
			toolCall = &call
		case EventFinish:
			f := event
			finish = &f
		case EventError:
			t.Fatalf("unexpected EventError: %v", event.Err)
		}
	}
	if textOut.String() != "Checking build... done." {
		t.Fatalf("streamed text = %q", textOut.String())
	}
	if toolCall == nil || toolCall.Name != "get_weather" {
		t.Fatalf("toolCall = %+v", toolCall)
	}
	if finish == nil || finish.FinishReason != "stop" {
		t.Fatalf("finish = %+v", finish)
	}
}

// TestGenerateNonCommittedMalformedDoesNotSendPartialPrompt 验证未提交可见内容时的重试仍使用原始请求且不附加续写提示
func TestGenerateNonCommittedMalformedDoesNotSendPartialPrompt(t *testing.T) {
	malformedCode := int64(10)
	stopCode := int64(1)

	attempt1 := playgroundStreamBody(t,
		playgroundStreamFrame([][]any{playgroundThoughtWirePart("internal reasoning")}, &malformedCode, "", nil),
	)
	attempt2 := playgroundStreamBody(t,
		playgroundStreamFrame([][]any{playgroundTextWirePart("Clean reply")}, &stopCode, "", &Usage{InputTokens: 8, OutputTokens: 3, TotalTokens: 11}),
	)

	client, transport := newRecoveryTestClient("gemini-test", attempt1, attempt2)
	stream, err := client.Generate(context.Background(), GenerateRequest{
		ID:        "req-noncommitted",
		AccountID: "acc@example.com",
		Model:     "gemini-test",
		Contents:  []Content{{Role: RoleUser, Parts: []Part{{Text: "Hi"}}}},
	})
	if err != nil {
		t.Fatalf("Generate failed: %v", err)
	}
	events := collectRecoveryEvents(stream)

	if len(transport.bodies) != 2 {
		t.Fatalf("attempts = %d, want 2", len(transport.bodies))
	}
	var wire []json.RawMessage
	if err := json.Unmarshal([]byte(transport.bodies[1]), &wire); err != nil {
		t.Fatalf("unmarshal retry wire body: %v", err)
	}
	var wireContents []json.RawMessage
	if err := json.Unmarshal(wire[1], &wireContents); err != nil {
		t.Fatalf("unmarshal retry contents: %v", err)
	}
	if len(wireContents) != 1 {
		t.Fatalf("non-committed retry contents len = %d, want 1", len(wireContents))
	}
	var textOut strings.Builder
	var finish *Event
	for _, event := range events {
		if event.Kind == EventText {
			textOut.WriteString(event.Text)
		}
		if event.Kind == EventFinish {
			f := event
			finish = &f
		}
		if event.Kind == EventError {
			t.Fatalf("unexpected EventError: %v", event.Err)
		}
	}
	if textOut.String() != "Clean reply" || finish == nil || finish.FinishReason != "stop" {
		t.Fatalf("text=%q finish=%+v", textOut.String(), finish)
	}
}

// TestGenerateCommittedNonTextDoesNotRecover 验证已提交函数调用后遇到异常终止不再续写恢复并直接上报 AbnormalFinishError
func TestGenerateCommittedNonTextDoesNotRecover(t *testing.T) {
	malformedCode := int64(10)
	attempt1 := playgroundStreamBody(t,
		playgroundStreamFrame([][]any{
			playgroundCallWirePart("call_1", "get_weather", map[string]any{"city": "London"}),
		}, &malformedCode, "second call broken", &Usage{InputTokens: 10, OutputTokens: 4, TotalTokens: 14}),
	)

	client, transport := newRecoveryTestClient("gemini-test", attempt1)
	stream, err := client.Generate(context.Background(), GenerateRequest{
		ID:        "req-committed-tool",
		AccountID: "acc@example.com",
		Model:     "gemini-test",
		Contents:  []Content{{Role: RoleUser, Parts: []Part{{Text: "Weather?"}}}},
		Tools: Tools{
			Functions: []FunctionDeclaration{{
				Name:       "get_weather",
				Parameters: json.RawMessage(`{"type":"object"}`),
			}},
		},
	})
	if err != nil {
		t.Fatalf("Generate failed: %v", err)
	}
	events := collectRecoveryEvents(stream)

	if len(transport.bodies) != 1 {
		t.Fatalf("expected 1 attempt without retry after committed tool call, got %d", len(transport.bodies))
	}
	var abnormal *AbnormalFinishError
	var sawFinish bool
	for _, event := range events {
		if event.Kind == EventFinish {
			sawFinish = true
		}
		if event.Kind == EventError {
			_ = errors.As(event.Err, &abnormal)
		}
	}
	if sawFinish {
		t.Fatalf("did not expect EventFinish")
	}
	if abnormal == nil || abnormal.Reason != "malformed_function_call" || abnormal.Message != "second call broken" {
		t.Fatalf("abnormal = %+v", abnormal)
	}
}
