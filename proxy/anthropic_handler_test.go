package proxy

import (
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
