package proxy

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/missdeer/aiproxy/config"
)

func TestConvertOpenAIChatToAnthropicRequest(t *testing.T) {
	tests := []struct {
		name             string
		input            map[string]any
		defaultMaxTokens []int
		wantMaxTokens    int
		wantSystem       string
		wantMsgCount     int
	}{
		{
			name: "basic conversion with max_tokens",
			input: map[string]any{
				"model":      "gpt-4",
				"max_tokens": 1000,
				"messages": []any{
					map[string]any{"role": "user", "content": "Hello"},
				},
			},
			wantMaxTokens: 1000,
			wantMsgCount:  1,
		},
		{
			name: "default max_tokens when not specified",
			input: map[string]any{
				"model": "gpt-4",
				"messages": []any{
					map[string]any{"role": "user", "content": "Hello"},
				},
			},
			wantMaxTokens: 4096,
			wantMsgCount:  1,
		},
		{
			name: "custom default max_tokens",
			input: map[string]any{
				"model": "gpt-4",
				"messages": []any{
					map[string]any{"role": "user", "content": "Hello"},
				},
			},
			defaultMaxTokens: []int{8192},
			wantMaxTokens:    8192,
			wantMsgCount:     1,
		},
		{
			name: "system message extraction",
			input: map[string]any{
				"model": "gpt-4",
				"messages": []any{
					map[string]any{"role": "system", "content": "You are helpful"},
					map[string]any{"role": "user", "content": "Hello"},
				},
			},
			wantMaxTokens: 4096,
			wantSystem:    "You are helpful",
			wantMsgCount:  1, // system message is extracted, not in messages
		},
		{
			name: "temperature and top_p",
			input: map[string]any{
				"model":       "gpt-4",
				"temperature": 0.7,
				"top_p":       0.9,
				"messages": []any{
					map[string]any{"role": "user", "content": "Hello"},
				},
			},
			wantMaxTokens: 4096,
			wantMsgCount:  1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := convertOpenAIChatToAnthropicRequest(tt.input, tt.defaultMaxTokens...)

			// Check max_tokens
			if maxTokens, ok := result["max_tokens"].(int); ok {
				if maxTokens != tt.wantMaxTokens {
					t.Errorf("max_tokens = %d, want %d", maxTokens, tt.wantMaxTokens)
				}
			} else {
				t.Error("max_tokens not found or not an int")
			}

			// Check system
			if tt.wantSystem != "" {
				if system, ok := result["system"].(string); ok {
					if system != tt.wantSystem {
						t.Errorf("system = %q, want %q", system, tt.wantSystem)
					}
				} else {
					t.Error("system not found")
				}
			}

			// Check messages count
			if messages, ok := result["messages"].([]map[string]any); ok {
				if len(messages) != tt.wantMsgCount {
					t.Errorf("messages count = %d, want %d", len(messages), tt.wantMsgCount)
				}
			}
		})
	}
}

func TestConvertAnthropicToOpenAIRequest(t *testing.T) {
	tests := []struct {
		name          string
		input         map[string]any
		wantMsgCount  int
		wantHasSystem bool
	}{
		{
			name: "basic conversion",
			input: map[string]any{
				"model":      "claude-3",
				"max_tokens": 1000,
				"messages": []any{
					map[string]any{"role": "user", "content": "Hello"},
				},
			},
			wantMsgCount: 1,
		},
		{
			name: "with system message",
			input: map[string]any{
				"model":      "claude-3",
				"max_tokens": 1000,
				"system":     "You are helpful",
				"messages": []any{
					map[string]any{"role": "user", "content": "Hello"},
				},
			},
			wantMsgCount:  2, // system + user
			wantHasSystem: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := convertAnthropicToOpenAIRequest(tt.input)

			if messages, ok := result["messages"].([]map[string]any); ok {
				if len(messages) != tt.wantMsgCount {
					t.Errorf("messages count = %d, want %d", len(messages), tt.wantMsgCount)
				}

				if tt.wantHasSystem && len(messages) > 0 {
					if messages[0]["role"] != "system" {
						t.Error("expected first message to be system")
					}
				}
			}
		})
	}
}

func TestConvertAnthropicToGeminiRequest(t *testing.T) {
	input := map[string]any{
		"model":      "claude-3",
		"max_tokens": 1000,
		"system":     "You are helpful",
		"messages": []any{
			map[string]any{"role": "user", "content": "Hello"},
			map[string]any{"role": "assistant", "content": "Hi there"},
		},
	}

	result := convertAnthropicToGeminiRequest(input)

	// Check contents
	if contents, ok := result["contents"].([]map[string]any); ok {
		if len(contents) != 2 {
			t.Errorf("contents count = %d, want 2", len(contents))
		}
		if contents[0]["role"] != "user" {
			t.Errorf("first role = %v, want user", contents[0]["role"])
		}
		if contents[1]["role"] != "model" {
			t.Errorf("second role = %v, want model", contents[1]["role"])
		}
	} else {
		t.Error("contents not found")
	}

	// Check systemInstruction
	if sysInstr, ok := result["systemInstruction"].(map[string]any); ok {
		if parts, ok := sysInstr["parts"].([]map[string]any); ok {
			if parts[0]["text"] != "You are helpful" {
				t.Error("system instruction text mismatch")
			}
		}
	} else {
		t.Error("systemInstruction not found")
	}

	// Check generationConfig
	if genConfig, ok := result["generationConfig"].(map[string]any); ok {
		if genConfig["maxOutputTokens"] != 1000 {
			t.Errorf("maxOutputTokens = %v, want 1000", genConfig["maxOutputTokens"])
		}
	} else {
		t.Error("generationConfig not found")
	}
}

func TestMapOpenAIStopReasonToAnthropic(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"stop", "end_turn"},
		{"length", "max_tokens"},
		{"tool_calls", "tool_use"},
		{"unknown", "end_turn"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			if got := mapOpenAIStopReasonToAnthropic(tt.input); got != tt.want {
				t.Errorf("mapOpenAIStopReasonToAnthropic(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestMapGeminiStopReasonToAnthropic(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"STOP", "end_turn"},
		{"MAX_TOKENS", "max_tokens"},
		{"UNKNOWN", "end_turn"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			if got := mapGeminiStopReasonToAnthropic(tt.input); got != tt.want {
				t.Errorf("mapGeminiStopReasonToAnthropic(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestMapGeminiToOpenAIFinishReason(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"STOP", "stop"},
		{"MAX_TOKENS", "length"},
		{"UNKNOWN", "stop"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			if got := mapGeminiToOpenAIFinishReason(tt.input); got != tt.want {
				t.Errorf("mapGeminiToOpenAIFinishReason(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestConvertOpenAIResponseToAnthropic(t *testing.T) {
	openAIResp := OpenAIChatResponse{
		ID:      "chatcmpl-123",
		Object:  "chat.completion",
		Created: 1234567890,
		Model:   "gpt-4",
		Choices: []OpenAIChoice{
			{
				Index: 0,
				Message: &OpenAIMessage{
					Role:    "assistant",
					Content: "Hello, how can I help?",
				},
				FinishReason: strPtr("stop"),
			},
		},
		Usage: OpenAIUsage{
			PromptTokens:     10,
			CompletionTokens: 20,
			TotalTokens:      30,
		},
	}

	body, _ := json.Marshal(openAIResp)
	result, err := convertOpenAIResponseToAnthropic(body)
	if err != nil {
		t.Fatalf("convertOpenAIResponseToAnthropic() error = %v", err)
	}

	var anthropicResp map[string]any
	if err := json.Unmarshal(result, &anthropicResp); err != nil {
		t.Fatalf("failed to unmarshal result: %v", err)
	}

	if anthropicResp["type"] != "message" {
		t.Errorf("type = %v, want message", anthropicResp["type"])
	}
	if anthropicResp["role"] != "assistant" {
		t.Errorf("role = %v, want assistant", anthropicResp["role"])
	}
	if anthropicResp["stop_reason"] != "end_turn" {
		t.Errorf("stop_reason = %v, want end_turn", anthropicResp["stop_reason"])
	}
}

func TestConvertResponseToOpenAI(t *testing.T) {
	// Test Gemini response conversion
	geminiResp := GeminiResponse{
		Candidates: []GeminiCandidate{
			{
				Content: GeminiContent{
					Parts: []GeminiPart{{Text: "Hello from Gemini"}},
					Role:  "model",
				},
				FinishReason: "STOP",
			},
		},
		UsageMetadata: &GeminiUsage{
			PromptTokenCount:     10,
			CandidatesTokenCount: 20,
		},
		ModelVersion: "gemini-1.5-pro",
	}

	body, _ := json.Marshal(geminiResp)
	result, err := convertResponseToOpenAI(body, config.APITypeGemini)
	if err != nil {
		t.Fatalf("convertResponseToOpenAI() error = %v", err)
	}

	var openAIResp OpenAIChatResponse
	if err := json.Unmarshal(result, &openAIResp); err != nil {
		t.Fatalf("failed to unmarshal result: %v", err)
	}

	if openAIResp.Object != "chat.completion" {
		t.Errorf("object = %v, want chat.completion", openAIResp.Object)
	}
	if len(openAIResp.Choices) != 1 {
		t.Errorf("choices count = %d, want 1", len(openAIResp.Choices))
	}
	if openAIResp.Choices[0].Message.Content != "Hello from Gemini" {
		t.Errorf("content = %v, want 'Hello from Gemini'", openAIResp.Choices[0].Message.Content)
	}
}

func strPtr(s string) *string {
	return &s
}

func TestConvertResponseToAnthropic_FromResponses(t *testing.T) {
	responsesBody := []byte(`{
		"id":"resp_123",
		"object":"response",
		"created_at":123,
		"model":"gpt-4.1",
		"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Hello from responses"}]}],
		"usage":{"input_tokens":3,"output_tokens":5,"total_tokens":8}
	}`)

	got, err := convertResponseToAnthropic(responsesBody, config.APITypeResponses)
	if err != nil {
		t.Fatalf("convertResponseToAnthropic() error = %v", err)
	}

	var anthropicResp map[string]any
	if err := json.Unmarshal(got, &anthropicResp); err != nil {
		t.Fatalf("failed to unmarshal anthropic response: %v", err)
	}

	if anthropicResp["type"] != "message" {
		t.Fatalf("type = %v, want message", anthropicResp["type"])
	}
	content, ok := anthropicResp["content"].([]any)
	if !ok || len(content) == 0 {
		t.Fatalf("content missing: %#v", anthropicResp["content"])
	}
	first, ok := content[0].(map[string]any)
	if !ok {
		t.Fatalf("content[0] is not object: %#v", content[0])
	}
	if first["text"] != "Hello from responses" {
		t.Fatalf("content text = %v, want %q", first["text"], "Hello from responses")
	}
}

func TestConvertToGeminiResponse_FromResponses(t *testing.T) {
	responsesBody := []byte(`{
		"id":"resp_123",
		"object":"response",
		"created_at":123,
		"model":"gpt-4.1",
		"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Hello from responses"}]}],
		"usage":{"input_tokens":3,"output_tokens":5,"total_tokens":8}
	}`)

	got, err := convertToGeminiResponse(responsesBody, config.APITypeResponses)
	if err != nil {
		t.Fatalf("convertToGeminiResponse() error = %v", err)
	}

	var geminiResp GeminiResponse
	if err := json.Unmarshal(got, &geminiResp); err != nil {
		t.Fatalf("failed to unmarshal gemini response: %v", err)
	}
	if len(geminiResp.Candidates) == 0 || len(geminiResp.Candidates[0].Content.Parts) == 0 {
		t.Fatalf("missing gemini text parts: %#v", geminiResp)
	}
	if geminiResp.Candidates[0].Content.Parts[0].Text != "Hello from responses" {
		t.Fatalf("gemini text = %q, want %q", geminiResp.Candidates[0].Content.Parts[0].Text, "Hello from responses")
	}
}

func TestConvertStreamToOpenAI_FromResponses(t *testing.T) {
	sse := strings.Join([]string{
		`event: response.output_text.delta`,
		`data: {"type":"response.output_text.delta","delta":"Hello"}`,
		``,
		`event: response.output_text.delta`,
		`data: {"type":"response.output_text.delta","delta":" world"}`,
		``,
		`event: response.completed`,
		`data: {"type":"response.completed","response":{"id":"resp_123","model":"gpt-4.1","usage":{"input_tokens":3,"output_tokens":5}}}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")

	h := &OpenAIHandler{}
	streamOut, err := h.convertStreamToOpenAI(strings.NewReader(sse), config.APITypeResponses, false)
	if err != nil {
		t.Fatalf("convertStreamToOpenAI(stream) error = %v", err)
	}
	streamText := string(streamOut)
	if !strings.Contains(streamText, "chat.completion.chunk") {
		t.Fatalf("stream output missing chunk object: %s", streamText)
	}
	if !strings.Contains(streamText, "[DONE]") {
		t.Fatalf("stream output missing [DONE]: %s", streamText)
	}
	if !strings.Contains(streamText, "Hello") {
		t.Fatalf("stream output missing delta text: %s", streamText)
	}

	nonStreamOut, err := h.convertStreamToOpenAI(strings.NewReader(sse), config.APITypeResponses, true)
	if err != nil {
		t.Fatalf("convertStreamToOpenAI(non-stream) error = %v", err)
	}
	var openAIResp OpenAIChatResponse
	if err := json.Unmarshal(nonStreamOut, &openAIResp); err != nil {
		t.Fatalf("failed to unmarshal OpenAI response: %v", err)
	}
	if len(openAIResp.Choices) == 0 || openAIResp.Choices[0].Message == nil {
		t.Fatalf("missing OpenAI choices: %#v", openAIResp)
	}
	content, ok := openAIResp.Choices[0].Message.Content.(string)
	if !ok || content != "Hello world" {
		t.Fatalf("OpenAI content = %#v, want %q", openAIResp.Choices[0].Message.Content, "Hello world")
	}
}

func TestConvertStreamToAnthropic_FromResponses(t *testing.T) {
	sse := strings.Join([]string{
		`event: response.output_text.delta`,
		`data: {"type":"response.output_text.delta","delta":"Hello"}`,
		``,
		`event: response.output_text.delta`,
		`data: {"type":"response.output_text.delta","delta":" world"}`,
		``,
		`event: response.completed`,
		`data: {"type":"response.completed","response":{"id":"resp_123","model":"gpt-4.1","usage":{"input_tokens":3,"output_tokens":5}}}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")

	h := &AnthropicHandler{}
	streamOut, err := h.convertStreamToAnthropic(strings.NewReader(sse), config.APITypeResponses, false)
	if err != nil {
		t.Fatalf("convertStreamToAnthropic(stream) error = %v", err)
	}
	streamText := string(streamOut)
	if !strings.Contains(streamText, "content_block_delta") {
		t.Fatalf("stream output missing content delta events: %s", streamText)
	}
	if !strings.Contains(streamText, "Hello") {
		t.Fatalf("stream output missing delta text: %s", streamText)
	}

	nonStreamOut, err := h.convertStreamToAnthropic(strings.NewReader(sse), config.APITypeResponses, true)
	if err != nil {
		t.Fatalf("convertStreamToAnthropic(non-stream) error = %v", err)
	}
	var anthropicResp map[string]any
	if err := json.Unmarshal(nonStreamOut, &anthropicResp); err != nil {
		t.Fatalf("failed to unmarshal Anthropic response: %v", err)
	}
	contentAny, ok := anthropicResp["content"].([]any)
	if !ok || len(contentAny) == 0 {
		t.Fatalf("anthropic content missing: %#v", anthropicResp["content"])
	}
	first, ok := contentAny[0].(map[string]any)
	if !ok || first["text"] != "Hello world" {
		t.Fatalf("anthropic content text = %#v, want %q", first["text"], "Hello world")
	}
}

func TestConvertStreamToGemini_FromResponses(t *testing.T) {
	sse := strings.Join([]string{
		`event: response.output_text.delta`,
		`data: {"type":"response.output_text.delta","delta":"Hello"}`,
		``,
		`event: response.output_text.delta`,
		`data: {"type":"response.output_text.delta","delta":" world"}`,
		``,
		`event: response.completed`,
		`data: {"type":"response.completed","response":{"id":"resp_123","model":"gpt-4.1","usage":{"input_tokens":3,"output_tokens":5}}}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")

	h := &GeminiHandler{}
	out, err := h.convertStreamToGemini(strings.NewReader(sse), config.APITypeResponses)
	if err != nil {
		t.Fatalf("convertStreamToGemini() error = %v", err)
	}
	var geminiResp GeminiResponse
	if err := json.Unmarshal(out, &geminiResp); err != nil {
		t.Fatalf("failed to unmarshal Gemini response: %v", err)
	}
	if len(geminiResp.Candidates) == 0 || len(geminiResp.Candidates[0].Content.Parts) == 0 {
		t.Fatalf("missing gemini candidates/parts: %#v", geminiResp)
	}
	if geminiResp.Candidates[0].Content.Parts[0].Text != "Hello world" {
		t.Fatalf("gemini text = %q, want %q", geminiResp.Candidates[0].Content.Parts[0].Text, "Hello world")
	}
}

func TestExtractGeminiStreamData_ResponseWrapped(t *testing.T) {
	// Root-level candidates (standard Gemini)
	rootLevel := map[string]any{
		"modelVersion": "gemini-2.5-pro",
		"candidates": []any{
			map[string]any{
				"content": map[string]any{
					"parts": []any{
						map[string]any{"text": "root text"},
					},
				},
			},
		},
		"usageMetadata": map[string]any{
			"promptTokenCount":     float64(10),
			"candidatesTokenCount": float64(20),
		},
	}
	texts, model, in, out := extractGeminiStreamData(rootLevel)
	if len(texts) != 1 || texts[0] != "root text" {
		t.Fatalf("root-level: texts = %v, want [\"root text\"]", texts)
	}
	if model != "gemini-2.5-pro" {
		t.Fatalf("root-level: model = %q, want %q", model, "gemini-2.5-pro")
	}
	if in != 10 || out != 20 {
		t.Fatalf("root-level: tokens = (%d, %d), want (10, 20)", in, out)
	}

	// Response-wrapped candidates (Gemini CLI / Antigravity)
	wrapped := map[string]any{
		"response": map[string]any{
			"modelVersion": "gemini-3-pro",
			"candidates": []any{
				map[string]any{
					"content": map[string]any{
						"parts": []any{
							map[string]any{"text": "wrapped text"},
						},
					},
				},
			},
			"usageMetadata": map[string]any{
				"promptTokenCount":     float64(5),
				"candidatesTokenCount": float64(15),
			},
		},
	}
	texts2, model2, in2, out2 := extractGeminiStreamData(wrapped)
	if len(texts2) != 1 || texts2[0] != "wrapped text" {
		t.Fatalf("wrapped: texts = %v, want [\"wrapped text\"]", texts2)
	}
	if model2 != "gemini-3-pro" {
		t.Fatalf("wrapped: model = %q, want %q", model2, "gemini-3-pro")
	}
	if in2 != 5 || out2 != 15 {
		t.Fatalf("wrapped: tokens = (%d, %d), want (5, 15)", in2, out2)
	}
}

func TestConvertStreamToAnthropic_FromGeminiWrapped(t *testing.T) {
	// Simulate response-wrapped Gemini CLI SSE events
	sse := strings.Join([]string{
		`data: {"response":{"modelVersion":"gemini-3-pro","candidates":[{"content":{"parts":[{"text":"Hello"}]}}]}}`,
		``,
		`data: {"response":{"modelVersion":"gemini-3-pro","candidates":[{"content":{"parts":[{"text":" world"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":5}}}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")

	h := &AnthropicHandler{}

	// Streaming mode
	streamOut, err := h.convertStreamToAnthropic(strings.NewReader(sse), config.APITypeGemini, false)
	if err != nil {
		t.Fatalf("convertStreamToAnthropic(stream) error = %v", err)
	}
	streamText := string(streamOut)
	if !strings.Contains(streamText, "content_block_delta") {
		t.Fatalf("stream output missing content delta events: %s", streamText)
	}
	if !strings.Contains(streamText, "Hello") || !strings.Contains(streamText, " world") {
		t.Fatalf("stream output missing delta text: %s", streamText)
	}

	// Non-streaming mode
	nonStreamOut, err := h.convertStreamToAnthropic(strings.NewReader(sse), config.APITypeGemini, true)
	if err != nil {
		t.Fatalf("convertStreamToAnthropic(non-stream) error = %v", err)
	}
	var anthropicResp map[string]any
	if err := json.Unmarshal(nonStreamOut, &anthropicResp); err != nil {
		t.Fatalf("failed to unmarshal Anthropic response: %v", err)
	}
	contentAny, ok := anthropicResp["content"].([]any)
	if !ok || len(contentAny) == 0 {
		t.Fatalf("anthropic content missing: %#v", anthropicResp["content"])
	}
	first, ok := contentAny[0].(map[string]any)
	if !ok || first["text"] != "Hello world" {
		t.Fatalf("content text = %#v, want %q", first["text"], "Hello world")
	}
}
