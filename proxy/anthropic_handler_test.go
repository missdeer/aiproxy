package proxy

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// ── extractPromptPreview tests ─────────────────────────────────────────

func TestExtractPromptPreview(t *testing.T) {
	tests := []struct {
		name     string
		messages []any
		want     string
	}{
		{
			name:     "empty messages",
			messages: nil,
			want:     "(empty)",
		},
		{
			name: "string content",
			messages: []any{
				map[string]any{"role": "user", "content": "Hello there"},
			},
			want: "Hello there",
		},
		{
			name: "array content with text block",
			messages: []any{
				map[string]any{
					"role": "user",
					"content": []any{
						map[string]any{"type": "text", "text": "What is 2+2?"},
					},
				},
			},
			want: "What is 2+2?",
		},
		{
			name: "takes last message",
			messages: []any{
				map[string]any{"role": "user", "content": "First message"},
				map[string]any{"role": "assistant", "content": "Reply"},
				map[string]any{"role": "user", "content": "Second message"},
			},
			want: "Second message",
		},
		{
			name: "no content key",
			messages: []any{
				map[string]any{"role": "user"},
			},
			want: "(no content)",
		},
		{
			name: "non-map message",
			messages: []any{
				"invalid",
			},
			want: "(unknown format)",
		},
		{
			name: "long string is truncated",
			messages: []any{
				map[string]any{"role": "user", "content": strings.Repeat("a", 200)},
			},
			want: strings.Repeat("a", 100) + "...",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractPromptPreview(tt.messages)
			if got != tt.want {
				t.Fatalf("extractPromptPreview() = %q, want %q", got, tt.want)
			}
		})
	}
}

// ── truncateString tests ───────────────────────────────────────────────

func TestTruncateString(t *testing.T) {
	tests := []struct {
		name   string
		s      string
		maxLen int
		want   string
	}{
		{"short string", "hello", 10, "hello"},
		{"exact length", "hello", 5, "hello"},
		{"truncated", "hello world", 5, "hello..."},
		{"empty string", "", 5, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := truncateString(tt.s, tt.maxLen)
			if got != tt.want {
				t.Fatalf("truncateString(%q, %d) = %q, want %q", tt.s, tt.maxLen, got, tt.want)
			}
		})
	}
}

// ── stripHopByHopHeaders tests ─────────────────────────────────────────

func TestStripHopByHopHeaders(t *testing.T) {
	t.Run("removes standard hop-by-hop headers", func(t *testing.T) {
		h := http.Header{}
		h.Set("Connection", "keep-alive")
		h.Set("Keep-Alive", "timeout=5")
		h.Set("Transfer-Encoding", "chunked")
		h.Set("Content-Type", "application/json")
		h.Set("X-Custom", "value")

		stripHopByHopHeaders(h)

		if h.Get("Connection") != "" {
			t.Fatal("Connection header should be removed")
		}
		if h.Get("Keep-Alive") != "" {
			t.Fatal("Keep-Alive header should be removed")
		}
		if h.Get("Transfer-Encoding") != "" {
			t.Fatal("Transfer-Encoding header should be removed")
		}
		// Non-hop-by-hop headers should be preserved
		if h.Get("Content-Type") != "application/json" {
			t.Fatal("Content-Type should be preserved")
		}
		if h.Get("X-Custom") != "value" {
			t.Fatal("X-Custom should be preserved")
		}
	})

	t.Run("removes Connection-listed headers", func(t *testing.T) {
		h := http.Header{}
		h.Set("Connection", "X-Foo, X-Bar")
		h.Set("X-Foo", "foo-val")
		h.Set("X-Bar", "bar-val")
		h.Set("X-Keep", "keep-val")

		stripHopByHopHeaders(h)

		if h.Get("X-Foo") != "" {
			t.Fatal("X-Foo should be removed (listed in Connection)")
		}
		if h.Get("X-Bar") != "" {
			t.Fatal("X-Bar should be removed (listed in Connection)")
		}
		if h.Get("X-Keep") != "keep-val" {
			t.Fatal("X-Keep should be preserved")
		}
	})
}

// ── convertStreamToNonStream tests ─────────────────────────────────────

func TestConvertStreamToNonStream(t *testing.T) {
	t.Run("assembles full message from SSE events", func(t *testing.T) {
		sse := "event: message_start\n" +
			`data: {"type":"message_start","message":{"id":"msg-123","model":"claude-3","usage":{"input_tokens":10}}}` + "\n\n" +
			"event: content_block_start\n" +
			`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n" +
			"event: content_block_delta\n" +
			`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello"}}` + "\n\n" +
			"event: content_block_delta\n" +
			`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":" World"}}` + "\n\n" +
			"event: message_delta\n" +
			`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":5}}` + "\n\n" +
			"event: message_stop\n" +
			`data: {"type":"message_stop"}` + "\n\n"

		h := &AnthropicHandler{}
		result, err := h.convertStreamToNonStream(strings.NewReader(sse))
		if err != nil {
			t.Fatalf("convertStreamToNonStream() error = %v", err)
		}

		var resp map[string]any
		if err := json.Unmarshal(result, &resp); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}

		if resp["id"] != "msg-123" {
			t.Fatalf("id = %q, want %q", resp["id"], "msg-123")
		}
		if resp["model"] != "claude-3" {
			t.Fatalf("model = %q, want %q", resp["model"], "claude-3")
		}
		if resp["stop_reason"] != "end_turn" {
			t.Fatalf("stop_reason = %q, want %q", resp["stop_reason"], "end_turn")
		}

		content := resp["content"].([]any)
		if len(content) != 1 {
			t.Fatalf("content length = %d, want 1", len(content))
		}
		block := content[0].(map[string]any)
		if block["text"] != "Hello World" {
			t.Fatalf("text = %q, want %q", block["text"], "Hello World")
		}

		usage := resp["usage"].(map[string]any)
		if usage["input_tokens"].(float64) != 10 {
			t.Fatalf("input_tokens = %v, want 10", usage["input_tokens"])
		}
		if usage["output_tokens"].(float64) != 5 {
			t.Fatalf("output_tokens = %v, want 5", usage["output_tokens"])
		}
	})

	t.Run("handles thinking deltas", func(t *testing.T) {
		sse := "event: content_block_start\n" +
			`data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}` + "\n\n" +
			"event: content_block_delta\n" +
			`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"Let me think"}}` + "\n\n" +
			"event: content_block_delta\n" +
			`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":" about this"}}` + "\n\n" +
			"event: message_stop\n" +
			`data: {"type":"message_stop"}` + "\n\n"

		h := &AnthropicHandler{}
		result, err := h.convertStreamToNonStream(strings.NewReader(sse))
		if err != nil {
			t.Fatalf("error = %v", err)
		}

		var resp map[string]any
		json.Unmarshal(result, &resp)
		content := resp["content"].([]any)
		block := content[0].(map[string]any)
		if block["thinking"] != "Let me think about this" {
			t.Fatalf("thinking = %q, want %q", block["thinking"], "Let me think about this")
		}
	})

	t.Run("returns error on error event", func(t *testing.T) {
		sse := "event: error\n" +
			`data: {"type":"error","error":{"message":"rate limit exceeded"}}` + "\n\n"

		h := &AnthropicHandler{}
		_, err := h.convertStreamToNonStream(strings.NewReader(sse))
		if err == nil {
			t.Fatal("expected error for SSE error event")
		}
		if !strings.Contains(err.Error(), "rate limit exceeded") {
			t.Fatalf("error = %q, want to contain 'rate limit exceeded'", err.Error())
		}
	})

	t.Run("handles empty stream", func(t *testing.T) {
		h := &AnthropicHandler{}
		result, err := h.convertStreamToNonStream(strings.NewReader(""))
		if err != nil {
			t.Fatalf("error = %v", err)
		}
		var resp map[string]any
		json.Unmarshal(result, &resp)
		if resp["type"] != "message" {
			t.Fatalf("type = %q, want %q", resp["type"], "message")
		}
		if resp["role"] != "assistant" {
			t.Fatalf("role = %q, want %q", resp["role"], "assistant")
		}
	})
}
