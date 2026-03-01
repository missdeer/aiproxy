package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/missdeer/aiproxy/config"
)

func TestNormalizeChunkBasic(t *testing.T) {
	var prev string

	if got := normalizeChunk("hello", &prev); got != "hello" {
		t.Errorf("first chunk: got %q, want %q", got, "hello")
	}
	if got := normalizeChunk("hello world", &prev); got != " world" {
		t.Errorf("extension: got %q, want %q", got, " world")
	}
	if got := normalizeChunk("hello world", &prev); got != "" {
		t.Errorf("duplicate: got %q, want %q", got, "")
	}
	if got := normalizeChunk("new content", &prev); got != "new content" {
		t.Errorf("fresh: got %q, want %q", got, "new content")
	}
}

func TestNormalizeChunkEmpty(t *testing.T) {
	var prev string
	if got := normalizeChunk("", &prev); got != "" {
		t.Errorf("empty: got %q, want %q", got, "")
	}
}

func TestNormalizeChunkSubstring(t *testing.T) {
	prev := "hello world"
	if got := normalizeChunk("hello", &prev); got != "" {
		t.Errorf("substring: got %q, want %q", got, "")
	}
}

func TestNormalizeChunkOverlap(t *testing.T) {
	prev := "abc123"
	if got := normalizeChunk("123xyz", &prev); got != "xyz" {
		t.Errorf("overlap: got %q, want %q", got, "xyz")
	}
}

func TestCleanKiroToolSchema(t *testing.T) {
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"name": map[string]any{
				"type":                 "string",
				"additionalProperties": false,
			},
			"value": nil,
		},
		"required":             []any{},
		"additionalProperties": true,
	}

	cleanKiroToolSchema(schema)

	if _, exists := schema["additionalProperties"]; exists {
		t.Error("additionalProperties should be removed")
	}
	if _, exists := schema["required"]; exists {
		t.Error("empty required should be removed")
	}
	props := schema["properties"].(map[string]any)
	if _, exists := props["value"]; exists {
		t.Error("null value should be removed")
	}
	nameProp := props["name"].(map[string]any)
	if _, exists := nameProp["additionalProperties"]; exists {
		t.Error("nested additionalProperties should be removed")
	}
}

func TestCleanKiroToolSchemaPreservesNonEmpty(t *testing.T) {
	schema := map[string]any{
		"type":     "object",
		"required": []any{"name", "value"},
	}

	cleanKiroToolSchema(schema)

	if _, exists := schema["required"]; !exists {
		t.Error("non-empty required should be preserved")
	}
}

func TestKiroConversationIDDeterministic(t *testing.T) {
	id1 := kiroConversationID("model-a", "system", "hello")
	id2 := kiroConversationID("model-a", "system", "hello")
	if id1 != id2 {
		t.Errorf("same inputs should produce same ID: %s != %s", id1, id2)
	}

	id3 := kiroConversationID("model-b", "system", "hello")
	if id1 == id3 {
		t.Error("different model should produce different ID")
	}
}

func TestKiroEventStreamToSSEReader(t *testing.T) {
	var buf bytes.Buffer
	buf.Write(buildTestFrame("assistantResponseEvent", "event", []byte(`{"content":"hello "}`)))
	buf.Write(buildTestFrame("assistantResponseEvent", "event", []byte(`{"content":"world"}`)))

	reader := newKiroEventStreamToSSEReader(io.NopCloser(&buf))
	output, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}

	s := string(output)
	if !strings.Contains(s, "message_start") {
		t.Error("missing message_start")
	}
	if !strings.Contains(s, "content_block_start") {
		t.Error("missing content_block_start")
	}
	if !strings.Contains(s, `"text_delta"`) {
		t.Error("missing text_delta")
	}
	if !strings.Contains(s, "hello ") {
		t.Error("missing first content")
	}
	if !strings.Contains(s, "world") {
		t.Error("missing second content")
	}
	if !strings.Contains(s, "message_stop") {
		t.Error("missing message_stop")
	}
	if !strings.Contains(s, "message_delta") {
		t.Error("missing message_delta")
	}
	if !strings.Contains(s, "content_block_stop") {
		t.Error("missing content_block_stop")
	}
}

func TestKiroEventStreamToSSEReaderEmptyStreamEnvelope(t *testing.T) {
	reader := newKiroEventStreamToSSEReader(io.NopCloser(bytes.NewReader(nil)))
	output, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}

	s := string(output)
	if !strings.Contains(s, "message_start") {
		t.Error("missing message_start for empty stream")
	}
	if !strings.Contains(s, "message_delta") {
		t.Error("missing message_delta for empty stream")
	}
	if !strings.Contains(s, "message_stop") {
		t.Error("missing message_stop for empty stream")
	}
}

func TestKiroEventStreamToSSEReaderThinking(t *testing.T) {
	var buf bytes.Buffer
	buf.Write(buildTestFrame("reasoningContentEvent", "event", []byte(`{"text":"let me think"}`)))
	buf.Write(buildTestFrame("assistantResponseEvent", "event", []byte(`{"content":"answer"}`)))

	reader := newKiroEventStreamToSSEReader(io.NopCloser(&buf))
	output, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}

	s := string(output)
	if !strings.Contains(s, `"thinking_delta"`) {
		t.Error("missing thinking_delta")
	}
	if !strings.Contains(s, "let me think") {
		t.Error("missing thinking content")
	}
	if !strings.Contains(s, `"text_delta"`) {
		t.Error("missing text_delta after thinking")
	}
}

func TestKiroEventStreamToSSEReaderToolUse(t *testing.T) {
	var buf bytes.Buffer
	buf.Write(buildTestFrame("toolUseEvent", "event", []byte(`{"toolUseId":"tu_1","name":"read_file","input":"{\"path\":\"test.txt\"}"}`)))
	buf.Write(buildTestFrame("toolUseEvent", "event", []byte(`{"toolUseId":"tu_1","stop":true}`)))

	reader := newKiroEventStreamToSSEReader(io.NopCloser(&buf))
	output, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}

	s := string(output)
	if !strings.Contains(s, `"tool_use"`) {
		t.Error("missing tool_use block")
	}
	if !strings.Contains(s, "read_file") {
		t.Error("missing tool name")
	}
	if !strings.Contains(s, "input_json_delta") {
		t.Error("missing input_json_delta")
	}
}

func TestKiroEventStreamToSSEReaderClose(t *testing.T) {
	closed := false
	closer := &testCloser{
		Reader: bytes.NewReader(nil),
		closeFunc: func() error {
			closed = true
			return nil
		},
	}

	reader := newKiroEventStreamToSSEReader(closer)
	reader.Close()

	if !closed {
		t.Error("Close should propagate to underlying reader")
	}
}

type testCloser struct {
	io.Reader
	closeFunc func() error
}

func (c *testCloser) Close() error {
	return c.closeFunc()
}

func TestKiroEventStreamToSSEReaderError(t *testing.T) {
	var buf bytes.Buffer
	buf.Write(buildTestFrame("", "error", []byte(`{"message":"throttled"}`)))

	reader := newKiroEventStreamToSSEReader(io.NopCloser(&buf))
	_, err := io.ReadAll(reader)
	if err == nil {
		t.Fatal("expected error for error frame")
	}
	if !strings.Contains(err.Error(), "error") {
		t.Errorf("error should mention 'error': %v", err)
	}
}

func TestKiroParseFullResponse(t *testing.T) {
	var buf bytes.Buffer
	buf.Write(buildTestFrame("assistantResponseEvent", "event", []byte(`{"content":"hello"}`)))
	buf.Write(buildTestFrame("assistantResponseEvent", "event", []byte(`{"content":"hello world"}`)))

	result, err := kiroParseFullResponse(&buf)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var resp map[string]any
	if err := json.Unmarshal(result, &resp); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if resp["type"] != "message" {
		t.Errorf("type = %v, want message", resp["type"])
	}
	content := resp["content"].([]any)
	if len(content) == 0 {
		t.Fatal("empty content")
	}
	textBlock := content[0].(map[string]any)
	if textBlock["type"] != "text" {
		t.Errorf("content type = %v, want text", textBlock["type"])
	}
	if text, ok := textBlock["text"].(string); !ok || text != "hello world" {
		t.Errorf("text = %q, want %q", text, "hello world")
	}
}

func TestKiroLoadStorageGeneratesMachineID(t *testing.T) {
	tmpDir := t.TempDir()
	authPath := filepath.Join(tmpDir, "kiro-auth.json")
	initial := []byte(`{"access_token":"a","refresh_token":"r","auth_method":"social","region":"us-east-1"}`)
	if err := os.WriteFile(authPath, initial, 0644); err != nil {
		t.Fatalf("write auth file: %v", err)
	}

	storage, err := kiroLoadStorage(authPath)
	if err != nil {
		t.Fatalf("kiroLoadStorage: %v", err)
	}
	if storage.MachineId == "" {
		t.Fatal("expected machine_id to be generated")
	}

	data, err := os.ReadFile(authPath)
	if err != nil {
		t.Fatalf("read auth file: %v", err)
	}
	var persisted map[string]any
	if err := json.Unmarshal(data, &persisted); err != nil {
		t.Fatalf("unmarshal persisted auth file: %v", err)
	}
	if persisted["machine_id"] == "" {
		t.Fatal("expected generated machine_id to be persisted")
	}
}

func TestForwardToKiro_StreamModeReturnsAnthropicSSE(t *testing.T) {
	tmpDir := t.TempDir()
	authFile := filepath.Join(tmpDir, "kiro-auth.json")
	storage := &KiroTokenStorage{
		AccessToken:  "tok",
		RefreshToken: "ref",
		AuthMethod:   "social",
		Region:       "us-east-1",
		MachineId:    "mid-1",
	}
	data, _ := json.Marshal(storage)
	if err := os.WriteFile(authFile, data, 0644); err != nil {
		t.Fatalf("write auth file: %v", err)
	}
	kiroAuthManager.StoreEntry(authFile, storage, time.Now().Add(1*time.Hour), &http.Client{Timeout: 30 * time.Second})
	defer kiroAuthManager.DeleteEntry(authFile)

	var frameBuf bytes.Buffer
	frameBuf.Write(buildTestFrame("assistantResponseEvent", "event", []byte(`{"content":"hello"}`)))
	client := &http.Client{
		Transport: &mockRoundTripper{
			handler: func(req *http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusOK,
					Body:       io.NopCloser(bytes.NewReader(frameBuf.Bytes())),
					Header:     http.Header{"Content-Type": {"application/octet-stream"}},
				}, nil
			},
		},
	}

	upstream := config.Upstream{
		Name:      "kiro-test",
		AuthFiles: []string{authFile},
	}
	status, body, headers, err := ForwardToKiro(client, upstream, []byte(`{"model":"claude-sonnet-4","input":"hi"}`), true)
	if err != nil {
		t.Fatalf("ForwardToKiro(stream=true) error: %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("status=%d, want 200", status)
	}
	if !strings.Contains(headers.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("Content-Type=%q, want text/event-stream", headers.Get("Content-Type"))
	}
	streamText := string(body)
	if !strings.Contains(streamText, "message_start") {
		t.Fatal("expected message_start in stream response")
	}
	if !strings.Contains(streamText, `"text_delta"`) {
		t.Fatal("expected text_delta in stream response")
	}
	if !strings.Contains(streamText, "message_stop") {
		t.Fatal("expected message_stop in stream response")
	}
}

func TestForwardToKiro_AllEndpoints429ReturnsStatusBody(t *testing.T) {
	tmpDir := t.TempDir()
	authFile := filepath.Join(tmpDir, "kiro-auth.json")
	storage := &KiroTokenStorage{
		AccessToken:  "tok",
		RefreshToken: "ref",
		AuthMethod:   "social",
		Region:       "us-east-1",
		MachineId:    "mid-1",
	}
	data, _ := json.Marshal(storage)
	if err := os.WriteFile(authFile, data, 0644); err != nil {
		t.Fatalf("write auth file: %v", err)
	}
	kiroAuthManager.StoreEntry(authFile, storage, time.Now().Add(1*time.Hour), &http.Client{Timeout: 30 * time.Second})
	defer kiroAuthManager.DeleteEntry(authFile)

	callCount := 0
	client := &http.Client{
		Transport: &mockRoundTripper{
			handler: func(req *http.Request) (*http.Response, error) {
				callCount++
				return &http.Response{
					StatusCode: http.StatusTooManyRequests,
					Body:       io.NopCloser(strings.NewReader(`{"error":"rate_limited"}`)),
					Header:     http.Header{"Content-Type": {"application/json"}},
				}, nil
			},
		},
	}

	upstream := config.Upstream{
		Name:      "kiro-test",
		AuthFiles: []string{authFile},
	}
	status, body, headers, err := ForwardToKiro(client, upstream, []byte(`{"model":"claude-sonnet-4","input":"hi"}`), false)
	if err != nil {
		t.Fatalf("ForwardToKiro() unexpected error: %v", err)
	}
	if status != http.StatusTooManyRequests {
		t.Fatalf("status=%d, want 429", status)
	}
	if callCount != 2 {
		t.Fatalf("callCount=%d, want 2 (dual-endpoint fallback)", callCount)
	}
	if !strings.Contains(string(body), "rate_limited") {
		t.Fatalf("body=%q, expected rate_limited payload", string(body))
	}
	if !strings.Contains(headers.Get("Content-Type"), "application/json") {
		t.Fatalf("Content-Type=%q, expected application/json", headers.Get("Content-Type"))
	}
}

func TestForwardToKiroStream_AllEndpoints429ReturnsResponse(t *testing.T) {
	tmpDir := t.TempDir()
	authFile := filepath.Join(tmpDir, "kiro-auth.json")
	storage := &KiroTokenStorage{
		AccessToken:  "tok",
		RefreshToken: "ref",
		AuthMethod:   "social",
		Region:       "us-east-1",
		MachineId:    "mid-1",
	}
	data, _ := json.Marshal(storage)
	if err := os.WriteFile(authFile, data, 0644); err != nil {
		t.Fatalf("write auth file: %v", err)
	}
	kiroAuthManager.StoreEntry(authFile, storage, time.Now().Add(1*time.Hour), &http.Client{Timeout: 30 * time.Second})
	defer kiroAuthManager.DeleteEntry(authFile)

	client := &http.Client{
		Transport: &mockRoundTripper{
			handler: func(req *http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusTooManyRequests,
					Body:       io.NopCloser(strings.NewReader(`{"error":"rate_limited"}`)),
					Header:     http.Header{"Content-Type": {"application/json"}},
				}, nil
			},
		},
	}

	upstream := config.Upstream{
		Name:      "kiro-test",
		AuthFiles: []string{authFile},
	}
	resp, err := ForwardToKiroStream(client, upstream, []byte(`{"model":"claude-sonnet-4","input":"hi"}`), context.Background())
	if err != nil {
		t.Fatalf("ForwardToKiroStream() unexpected error: %v", err)
	}
	status, errBody, headers, streamResp, handleErr := HandleStreamResponse(resp)
	if handleErr != nil {
		t.Fatalf("HandleStreamResponse() error: %v", handleErr)
	}
	if streamResp != nil {
		t.Fatal("streamResp should be nil for 429 response")
	}
	if status != http.StatusTooManyRequests {
		t.Fatalf("status=%d, want 429", status)
	}
	if !strings.Contains(string(errBody), "rate_limited") {
		t.Fatalf("errBody=%q, expected rate_limited payload", string(errBody))
	}
	if !strings.Contains(headers.Get("Content-Type"), "application/json") {
		t.Fatalf("Content-Type=%q, expected application/json", headers.Get("Content-Type"))
	}
}

func TestBuildKiroPayload(t *testing.T) {
	input := `{
		"model": "claude-sonnet-4",
		"instructions": "You are helpful",
		"input": [
			{"role": "user", "content": "Hello"}
		],
		"max_output_tokens": 4096,
		"temperature": 0.5,
		"tools": [
			{
				"type": "function",
				"function": {
					"name": "read_file",
					"description": "Read a file",
					"parameters": {
						"type": "object",
						"properties": {"path": {"type": "string"}},
						"required": [],
						"additionalProperties": false
					}
				}
			}
		]
	}`

	ep := kiroEndpoint{Origin: "AI_EDITOR"}

	result, err := buildKiroPayload([]byte(input), ep)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var payload map[string]any
	if err := json.Unmarshal(result, &payload); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}

	cs := payload["conversationState"].(map[string]any)
	cm := cs["currentMessage"].(map[string]any)
	uim := cm["userInputMessage"].(map[string]any)

	content := uim["content"].(string)
	if !strings.Contains(content, "--- SYSTEM PROMPT ---") {
		t.Error("system prompt not inlined")
	}
	if !strings.Contains(content, "Hello") {
		t.Error("user content missing")
	}

	if uim["modelId"] != "claude-sonnet-4" {
		t.Errorf("modelId = %v", uim["modelId"])
	}

	ctx := uim["userInputMessageContext"].(map[string]any)
	tools := ctx["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(tools))
	}

	toolSpec := tools[0].(map[string]any)["toolSpecification"].(map[string]any)
	schema := toolSpec["inputSchema"].(map[string]any)["json"].(map[string]any)
	if _, exists := schema["required"]; exists {
		t.Error("empty required should be cleaned")
	}
	if _, exists := schema["additionalProperties"]; exists {
		t.Error("additionalProperties should be cleaned")
	}

	ic := payload["inferenceConfig"].(map[string]any)
	if ic["maxTokens"] != float64(4096) {
		t.Errorf("maxTokens = %v", ic["maxTokens"])
	}
	if ic["temperature"] != 0.5 {
		t.Errorf("temperature = %v", ic["temperature"])
	}
}

func TestBuildKiroPayloadToolResultFallbackID(t *testing.T) {
	input := `{
		"model": "claude-sonnet-4",
		"input": [
			{"role":"assistant","content":[{"type":"tool_use","id":"tu_1","name":"read_file","input":{"path":"a.txt"}}]},
			{"role":"tool","tool_call_id":"tu_1","content":"ok"},
			{"role":"user","content":"next"}
		]
	}`
	ep := kiroEndpoint{Origin: "AI_EDITOR"}

	result, err := buildKiroPayload([]byte(input), ep)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var payload map[string]any
	if err := json.Unmarshal(result, &payload); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}

	cs := payload["conversationState"].(map[string]any)
	cm := cs["currentMessage"].(map[string]any)
	uim := cm["userInputMessage"].(map[string]any)
	ctx := uim["userInputMessageContext"].(map[string]any)
	toolResults := ctx["toolResults"].([]any)
	if len(toolResults) != 1 {
		t.Fatalf("expected 1 tool result, got %d", len(toolResults))
	}
	tr := toolResults[0].(map[string]any)
	if tr["toolUseId"] != "tu_1" {
		t.Fatalf("toolUseId=%v, want tu_1", tr["toolUseId"])
	}
}

func TestBuildKiroPayloadFunctionCallItems(t *testing.T) {
	input := `{
		"model": "claude-sonnet-4",
		"input": [
			{"type":"function_call","call_id":"tu_1","name":"read_file","arguments":"{\"path\":\"a.txt\"}"},
			{"type":"function_call_output","call_id":"tu_1","output":{"ok":true}},
			{"role":"user","content":"next"}
		]
	}`
	ep := kiroEndpoint{Origin: "AI_EDITOR"}

	result, err := buildKiroPayload([]byte(input), ep)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var payload map[string]any
	if err := json.Unmarshal(result, &payload); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}

	cs := payload["conversationState"].(map[string]any)
	history := cs["history"].([]any)
	if len(history) == 0 {
		t.Fatal("expected assistant tool_use history entry")
	}
	assistantMsg := history[0].(map[string]any)["assistantResponseMessage"].(map[string]any)
	toolUses := assistantMsg["toolUses"].([]any)
	if len(toolUses) != 1 {
		t.Fatalf("expected 1 tool use, got %d", len(toolUses))
	}

	cm := cs["currentMessage"].(map[string]any)
	uim := cm["userInputMessage"].(map[string]any)
	ctx := uim["userInputMessageContext"].(map[string]any)
	toolResults := ctx["toolResults"].([]any)
	if len(toolResults) != 1 {
		t.Fatalf("expected 1 tool result, got %d", len(toolResults))
	}
	tr := toolResults[0].(map[string]any)
	if tr["toolUseId"] != "tu_1" {
		t.Fatalf("toolUseId=%v, want tu_1", tr["toolUseId"])
	}
	content := tr["content"].([]any)
	first := content[0].(map[string]any)
	if first["text"] != `{"ok":true}` {
		t.Fatalf("tool result text=%v, want JSON string", first["text"])
	}
}
