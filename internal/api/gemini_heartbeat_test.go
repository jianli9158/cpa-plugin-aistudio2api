package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGeminiSSEHeartbeatPreservesFrameBoundaries(t *testing.T) {
	for _, beats := range []int{1, 2, 3} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/v1beta/models/gemini-2.5-flash:streamGenerateContent?alt=sse", nil)
		stream, err := openGeminiStream(rec, req)
		if err != nil {
			t.Fatalf("beats=%d: openGeminiStream failed: %v", beats, err)
		}
		for range beats {
			stream.keepalive.beat()
		}
		if err := stream.write(map[string]any{
			"candidates": []map[string]any{{
				"index": 0,
				"content": map[string]any{
					"role":  "model",
					"parts": []map[string]any{{"text": "ok"}},
				},
				"finishReason": "STOP",
			}},
		}); err != nil {
			t.Fatalf("beats=%d: stream.write failed: %v", beats, err)
		}
		stream.close()

		body := rec.Body.String()
		if geminiHeartbeat != "\n\n" {
			t.Fatalf("geminiHeartbeat = %q, want %q", geminiHeartbeat, "\n\n")
		}
		blocks := strings.Split(body, "\n\n")
		parsed := 0
		for _, block := range blocks {
			trimmed := strings.TrimSpace(block)
			if trimmed == "" {
				continue
			}
			if !strings.HasPrefix(trimmed, "data: ") {
				t.Fatalf("beats=%d: SSE block corrupted (missing 'data: ' prefix): %q (full body=%q)", beats, trimmed, body)
			}
			payload := strings.TrimPrefix(trimmed, "data: ")
			var decoded map[string]any
			if err := json.Unmarshal([]byte(payload), &decoded); err != nil {
				t.Fatalf("beats=%d: invalid JSON in SSE event %q: %v", beats, payload, err)
			}
			parsed++
		}
		if parsed != 1 {
			t.Fatalf("beats=%d: expected 1 parsed SSE event, got %d (body=%q)", beats, parsed, body)
		}
	}
}
