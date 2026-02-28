package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/missdeer/aiproxy/balancer"
	"github.com/missdeer/aiproxy/config"
)

// ── PipeStream tests ──────────────────────────────────────────────────

func TestPipeStream_CleanCompletion(t *testing.T) {
	data := "data: chunk1\n\ndata: chunk2\n\ndata: [DONE]\n\n"
	src := strings.NewReader(data)

	rec := httptest.NewRecorder()
	result := PipeStream(rec, src)

	if !result.OK() {
		t.Fatalf("expected OK, got UpstreamErr=%v DownstreamErr=%v", result.UpstreamErr, result.DownstreamErr)
	}
	if result.BytesWritten != int64(len(data)) {
		t.Fatalf("BytesWritten = %d, want %d", result.BytesWritten, len(data))
	}
	if rec.Body.String() != data {
		t.Fatalf("body = %q, want %q", rec.Body.String(), data)
	}
}

func TestPipeStream_UpstreamError(t *testing.T) {
	errBoom := errors.New("upstream exploded")
	src := &failingReader{data: []byte("partial"), err: errBoom}

	rec := httptest.NewRecorder()
	result := PipeStream(rec, src)

	if result.UpstreamErr != errBoom {
		t.Fatalf("UpstreamErr = %v, want %v", result.UpstreamErr, errBoom)
	}
	if result.DownstreamErr != nil {
		t.Fatalf("DownstreamErr = %v, want nil", result.DownstreamErr)
	}
	// Partial data should still be written
	if result.BytesWritten != int64(len("partial")) {
		t.Fatalf("BytesWritten = %d, want %d", result.BytesWritten, len("partial"))
	}
}

func TestPipeStream_DownstreamError(t *testing.T) {
	data := "data: chunk1\n\ndata: chunk2\n\n"
	src := strings.NewReader(data)

	errWrite := errors.New("broken pipe")
	w := &failingWriter{err: errWrite}

	result := PipeStream(w, src)

	if result.DownstreamErr != errWrite {
		t.Fatalf("DownstreamErr = %v, want %v", result.DownstreamErr, errWrite)
	}
	if result.UpstreamErr != nil {
		t.Fatalf("UpstreamErr = %v, want nil", result.UpstreamErr)
	}
}

// ── doHTTPRequestStream tests ─────────────────────────────────────────

func TestDoHTTPRequestStream_ReturnsUnreadBody(t *testing.T) {
	chunks := []string{"data: chunk1\n\n", "data: chunk2\n\n", "data: [DONE]\n\n"}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		for _, c := range chunks {
			w.Write([]byte(c))
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
	}))
	defer upstream.Close()

	originalReq := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	u := config.Upstream{BaseURL: upstream.URL, Token: "test-token", APIType: config.APITypeOpenAI}

	resp, err := doHTTPRequestStream(upstream.Client(), upstream.URL, []byte(`{}`), u, config.APITypeOpenAI, originalReq, "")
	if err != nil {
		t.Fatalf("doHTTPRequestStream error: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}

	// Body should be readable (not pre-read)
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("ReadAll error: %v", err)
	}
	expected := strings.Join(chunks, "")
	if string(body) != expected {
		t.Fatalf("body = %q, want %q", string(body), expected)
	}
}

func TestDoHTTPRequestStream_SetsAuthHeaders(t *testing.T) {
	var gotAuth string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	originalReq := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	u := config.Upstream{BaseURL: upstream.URL, Token: "sk-test-123", APIType: config.APITypeOpenAI}

	resp, err := doHTTPRequestStream(upstream.Client(), upstream.URL, []byte(`{}`), u, config.APITypeOpenAI, originalReq, "")
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	resp.Body.Close()

	if gotAuth != "Bearer sk-test-123" {
		t.Fatalf("Authorization = %q, want %q", gotAuth, "Bearer sk-test-123")
	}
}

func TestDoHTTPRequestStream_ErrorStatus(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte(`{"error":"rate limited"}`))
	}))
	defer upstream.Close()

	originalReq := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	u := config.Upstream{BaseURL: upstream.URL, Token: "test", APIType: config.APITypeOpenAI}

	resp, err := doHTTPRequestStream(upstream.Client(), upstream.URL, []byte(`{}`), u, config.APITypeOpenAI, originalReq, "")
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusTooManyRequests)
	}
	// Body should still be readable for error logging
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "rate limited") {
		t.Fatalf("body = %q, want to contain 'rate limited'", string(body))
	}
}

func TestDoHTTPRequestStream_ContextCancellation(t *testing.T) {
	var serverCtxErr atomic.Value
	started := make(chan struct{})

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("data: chunk1\n\n"))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		close(started)
		// Block until context is cancelled
		<-r.Context().Done()
		serverCtxErr.Store(r.Context().Err())
	}))
	defer upstream.Close()

	ctx, cancel := context.WithCancel(context.Background())
	originalReq := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	originalReq = originalReq.WithContext(ctx)
	u := config.Upstream{BaseURL: upstream.URL, Token: "test", APIType: config.APITypeOpenAI}

	resp, err := doHTTPRequestStream(upstream.Client(), upstream.URL, []byte(`{}`), u, config.APITypeOpenAI, originalReq, "")
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	defer resp.Body.Close()

	// Read to trigger server to get started
	buf := make([]byte, 1024)
	resp.Body.Read(buf)
	<-started

	// Cancel the context
	cancel()

	// Reading more should eventually fail
	for i := 0; i < 100; i++ {
		_, readErr := resp.Body.Read(buf)
		if readErr != nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Server should have received the cancellation
	time.Sleep(100 * time.Millisecond)
	if v := serverCtxErr.Load(); v == nil {
		t.Fatal("server context was not cancelled")
	}
}

// ── End-to-end streaming passthrough test ──────────────────────────────

func TestStreamingPassthrough_OpenAI(t *testing.T) {
	// Verify true streaming: client receives first chunk while upstream
	// is still blocked waiting to send chunk 2. Uses channel synchronization
	// instead of timing assertions for deterministic behavior.
	chunks := []string{
		"data: {\"choices\":[{\"delta\":{\"content\":\"Hello\"}}]}\n\n",
		"data: {\"choices\":[{\"delta\":{\"content\":\" world\"}}]}\n\n",
		"data: [DONE]\n\n",
	}
	// firstChunkFlushed is closed after the upstream writes+flushes chunk 1.
	// proceedToChunk2 is closed by the client after it reads chunk 1,
	// proving incremental delivery.
	firstChunkFlushed := make(chan struct{})
	proceedToChunk2 := make(chan struct{})

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)

		flusher, _ := w.(http.Flusher)

		// Send chunk 1 and signal
		w.Write([]byte(chunks[0]))
		flusher.Flush()
		close(firstChunkFlushed)

		// Block until client confirms it received chunk 1
		<-proceedToChunk2

		// Send remaining chunks
		for _, chunk := range chunks[1:] {
			w.Write([]byte(chunk))
			flusher.Flush()
		}
	}))
	defer upstream.Close()

	cfg := &config.Config{
		Upstreams: []config.Upstream{{
			Name: "test-openai", BaseURL: upstream.URL, Token: "test-token",
			Weight: 1, APIType: config.APITypeOpenAI, AvailableModels: []string{"gpt-4o-mini"},
		}},
	}
	handler := NewOpenAIHandler(cfg)
	proxy := httptest.NewServer(handler)
	defer proxy.Close()

	reqBody := `{"model":"gpt-4o-mini","messages":[{"role":"user","content":"Hi"}],"stream":true}`
	resp, err := http.Post(proxy.URL+"/v1/chat/completions", "application/json", strings.NewReader(reqBody))
	if err != nil {
		t.Fatalf("POST error: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}

	// Wait for upstream to flush chunk 1
	<-firstChunkFlushed

	// Read the first chunk from the client side.
	// If the proxy is buffering, this Read would block forever because
	// the upstream is waiting on proceedToChunk2.
	buf := make([]byte, 4096)
	n, readErr := resp.Body.Read(buf)
	if readErr != nil && readErr != io.EOF {
		t.Fatalf("first read error: %v", readErr)
	}
	if n == 0 {
		t.Fatal("first read returned 0 bytes — proxy may be buffering")
	}
	gotFirst := string(buf[:n])
	if !strings.Contains(gotFirst, "Hello") {
		t.Fatalf("first chunk = %q, want to contain 'Hello'", gotFirst)
	}

	// Signal upstream to send remaining chunks
	close(proceedToChunk2)

	// Read the rest
	var allData bytes.Buffer
	allData.WriteString(gotFirst)
	for {
		n, readErr = resp.Body.Read(buf)
		if n > 0 {
			allData.Write(buf[:n])
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			t.Fatalf("read error: %v", readErr)
		}
	}

	expected := strings.Join(chunks, "")
	if allData.String() != expected {
		t.Fatalf("body = %q, want %q", allData.String(), expected)
	}
}

func TestStreamingPassthrough_BalancerRecording(t *testing.T) {
	// Test that balancer records success after clean stream completion.
	// Strategy: pre-load 2 failures (threshold is 3). After a successful stream,
	// RecordSuccess should reset the count to 0. We verify by checking that
	// it takes 3 more failures (not 1) to reach the unavailable threshold.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("data: test\n\ndata: [DONE]\n\n"))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}))
	defer upstream.Close()

	upstreams := []config.Upstream{
		{
			Name:            "test-upstream",
			BaseURL:         upstream.URL,
			Token:           "test",
			Weight:          1,
			APIType:         config.APITypeOpenAI,
			AvailableModels: []string{"gpt-4o-mini"},
		},
	}
	bal := balancer.NewWeightedRoundRobin(upstreams)

	cfg := &config.Config{Upstreams: upstreams}
	handler := NewOpenAIHandler(cfg)
	handler.balancer = bal

	// Pre-load 2 failures (just under threshold of 3)
	bal.RecordFailure("test-upstream", "gpt-4o-mini")
	bal.RecordFailure("test-upstream", "gpt-4o-mini")

	proxy := httptest.NewServer(handler)
	defer proxy.Close()

	reqBody := `{"model":"gpt-4o-mini","messages":[{"role":"user","content":"Hi"}],"stream":true}`
	resp, err := http.Post(proxy.URL+"/v1/chat/completions", "application/json", strings.NewReader(reqBody))
	if err != nil {
		t.Fatalf("POST error: %v", err)
	}
	io.ReadAll(resp.Body)
	resp.Body.Close()

	// If RecordSuccess was called, failures should be reset to 0.
	// So it should take 3 more failures to reach the threshold.
	// If RecordSuccess was NOT called, we'd only need 1 more failure.
	bal.RecordFailure("test-upstream", "gpt-4o-mini") // failure count: 1
	bal.RecordFailure("test-upstream", "gpt-4o-mini") // failure count: 2
	if !bal.IsAvailable("test-upstream", "gpt-4o-mini") {
		t.Fatal("upstream became unavailable after 2 failures — RecordSuccess did not reset the count")
	}
	markedUnavailable := bal.RecordFailure("test-upstream", "gpt-4o-mini") // failure count: 3 → threshold
	if !markedUnavailable {
		t.Fatal("expected upstream to become unavailable after 3 failures (threshold)")
	}
}

func TestStreamingPassthrough_ClientDisconnect_NoBalancerRecord(t *testing.T) {
	// Test that client disconnect does NOT trigger RecordSuccess or RecordFailure
	started := make(chan struct{})

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		w.Write([]byte("data: first\n\n"))
		flusher.Flush()
		close(started)
		// Keep streaming until context cancelled
		for {
			select {
			case <-r.Context().Done():
				return
			case <-time.After(10 * time.Millisecond):
				w.Write([]byte(fmt.Sprintf("data: tick %d\n\n", time.Now().UnixNano())))
				flusher.Flush()
			}
		}
	}))
	defer upstream.Close()

	upstreams := []config.Upstream{
		{
			Name:            "test-upstream",
			BaseURL:         upstream.URL,
			Token:           "test",
			Weight:          1,
			APIType:         config.APITypeOpenAI,
			AvailableModels: []string{"gpt-4o-mini"},
		},
	}
	bal := balancer.NewWeightedRoundRobin(upstreams)

	cfg := &config.Config{Upstreams: upstreams}
	handler := NewOpenAIHandler(cfg)
	handler.balancer = bal

	// Pre-record a failure so we can detect if RecordSuccess clears it
	bal.RecordFailure("test-upstream", "gpt-4o-mini")

	proxy := httptest.NewServer(handler)
	defer proxy.Close()

	// Create a request with a cancellable context
	reqBody := `{"model":"gpt-4o-mini","messages":[{"role":"user","content":"Hi"}],"stream":true}`
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, proxy.URL+"/v1/chat/completions", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request error: %v", err)
	}

	// Read first chunk to confirm streaming started
	buf := make([]byte, 4096)
	resp.Body.Read(buf)
	<-started

	// Cancel the request (simulating client disconnect)
	cancel()
	resp.Body.Close()

	// Give the handler time to process the disconnect
	time.Sleep(200 * time.Millisecond)

	// Disconnect should trigger no record at all (neither success nor failure).
	// Pre-recorded count was 1. If disconnect added a failure, count is now 2.
	// If disconnect added a success, count is now 0.
	// Correct behavior: count remains 1.
	//
	// Assert by adding failures and checking return values:
	// - 1st added failure: count 1→2, should return false (not yet at threshold 3)
	// - 2nd added failure: count 2→3, should return true (threshold reached)
	// If disconnect incorrectly called RecordFailure, count would be 2→3 on first add (returns true).
	// If disconnect incorrectly called RecordSuccess, count would be 0→1 on first add, 1→2 on second (returns false).
	firstTripped := bal.RecordFailure("test-upstream", "gpt-4o-mini")
	if firstTripped {
		t.Fatal("first added failure tripped threshold — disconnect incorrectly called RecordFailure")
	}
	secondTripped := bal.RecordFailure("test-upstream", "gpt-4o-mini")
	if !secondTripped {
		t.Fatal("second added failure did not trip threshold — disconnect incorrectly called RecordSuccess (count was reset)")
	}
}

func TestStreamingPassthrough_NonStreamingUnchanged(t *testing.T) {
	// Verify non-streaming requests still use the buffered path
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]any{
			"id":      "chatcmpl-123",
			"object":  "chat.completion",
			"model":   body["model"],
			"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": "Hi!"}, "finish_reason": "stop", "index": 0}},
			"usage":   map[string]any{"prompt_tokens": 5, "completion_tokens": 2, "total_tokens": 7},
		})
	}))
	defer upstream.Close()

	cfg := &config.Config{
		Upstreams: []config.Upstream{
			{
				Name:            "test-openai",
				BaseURL:         upstream.URL,
				Token:           "test-token",
				Weight:          1,
				APIType:         config.APITypeOpenAI,
				AvailableModels: []string{"gpt-4o-mini"},
			},
		},
	}
	handler := NewOpenAIHandler(cfg)
	proxy := httptest.NewServer(handler)
	defer proxy.Close()

	// Non-streaming request (stream: false or absent)
	reqBody := `{"model":"gpt-4o-mini","messages":[{"role":"user","content":"Hi"}]}`
	resp, err := http.Post(proxy.URL+"/v1/chat/completions", "application/json", strings.NewReader(reqBody))
	if err != nil {
		t.Fatalf("POST error: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}

	var result map[string]any
	json.NewDecoder(resp.Body).Decode(&result)
	if result["object"] != "chat.completion" {
		t.Fatalf("object = %v, want chat.completion", result["object"])
	}
}

// ── HandleStreamResponse tests ──────────────────────────────────────

func TestHandleStreamResponse_Success(t *testing.T) {
	// Verify that a 200 response is returned as a streamResp with body unconsumed.
	body := "data: hello\n\ndata: world\n\n"
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header: http.Header{
			"Content-Type": {"text/event-stream"},
		},
		Body: io.NopCloser(strings.NewReader(body)),
	}

	status, errBody, headers, streamResp, err := HandleStreamResponse(resp)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("status = %d, want %d", status, http.StatusOK)
	}
	if errBody != nil {
		t.Fatalf("errBody should be nil, got %q", errBody)
	}
	if headers != nil {
		t.Fatal("headers should be nil for success path")
	}
	if streamResp == nil {
		t.Fatal("streamResp should not be nil")
	}
	defer streamResp.Body.Close()

	// Body should be unconsumed and readable
	got, _ := io.ReadAll(streamResp.Body)
	if string(got) != body {
		t.Fatalf("body = %q, want %q", string(got), body)
	}
}

func TestHandleStreamResponse_Error(t *testing.T) {
	// Verify that a 4xx response is buffered and returned as errBody.
	errJSON := `{"error": "rate limited"}`
	resp := &http.Response{
		StatusCode: http.StatusTooManyRequests,
		Header: http.Header{
			"Content-Type": {"application/json"},
			"Connection":   {"keep-alive"},
		},
		Body: io.NopCloser(strings.NewReader(errJSON)),
	}

	status, errBody, headers, streamResp, err := HandleStreamResponse(resp)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want %d", status, http.StatusTooManyRequests)
	}
	if string(errBody) != errJSON {
		t.Fatalf("errBody = %q, want %q", errBody, errJSON)
	}
	if headers == nil {
		t.Fatal("headers should not be nil for error path")
	}
	// Hop-by-hop headers should be stripped
	if headers.Get("Connection") != "" {
		t.Fatal("Connection header should be stripped")
	}
	if streamResp != nil {
		t.Fatal("streamResp should be nil for error path")
	}
}

func TestHandleStreamResponse_ErrorReadFailure(t *testing.T) {
	readErr := errors.New("read failed")
	resp := &http.Response{
		StatusCode: http.StatusBadGateway,
		Header: http.Header{
			"Content-Type": {"application/json"},
		},
		Body: io.NopCloser(&failingReader{data: []byte("partial"), err: readErr}),
	}

	status, errBody, headers, streamResp, err := HandleStreamResponse(resp)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "failed to read upstream error response body") {
		t.Fatalf("error = %v, want wrapped read-body error", err)
	}
	if !errors.Is(err, readErr) {
		t.Fatalf("error = %v, want wrapped %v", err, readErr)
	}
	if status != 0 {
		t.Fatalf("status = %d, want 0", status)
	}
	if errBody != nil {
		t.Fatalf("errBody = %q, want nil", errBody)
	}
	if headers != nil {
		t.Fatal("headers should be nil on read failure")
	}
	if streamResp != nil {
		t.Fatal("streamResp should be nil on read failure")
	}
}

// ── StreamCapableSender type assertion tests ──────────────────────────

func TestStreamCapableSender_TypeAssertions(t *testing.T) {
	// Verify that exactly the 4 target senders implement StreamCapableSender
	// and the 4 non-target senders don't.
	tests := []struct {
		name     string
		sender   OutboundSender
		expected bool
	}{
		{"CodexSender", &CodexSender{}, true},
		{"GeminiCLISender", &GeminiCLISender{}, true},
		{"AntigravitySender", &AntigravitySender{}, true},
		{"ClaudeCodeSender", &ClaudeCodeSender{}, true},
		{"OpenAISender", &OpenAISender{}, false},
		{"AnthropicSender", &AnthropicSender{}, false},
		{"GeminiSender", &GeminiSender{}, false},
		{"ResponsesSender", &ResponsesSender{}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, ok := tt.sender.(StreamCapableSender)
			if ok != tt.expected {
				t.Fatalf("StreamCapableSender type assertion = %v, want %v", ok, tt.expected)
			}
		})
	}
}

// ── E2E streaming fast path tests ─────────────────────────────────────
// These tests verify the full HandleStreamResponse → PipeStream chain
// delivers chunks incrementally, proving true streaming passthrough.
// Each test simulates a format-compatible upstream using channel
// synchronization (same pattern as TestStreamingPassthrough_OpenAI)
// to prove the first chunk arrives while the upstream is still blocked.

func testStreamingFastPathE2E(t *testing.T, label string, respFormat config.APIType) {
	t.Helper()

	chunks := []string{
		"data: {\"type\":\"chunk1\"}\n\n",
		"data: {\"type\":\"chunk2\"}\n\n",
		"data: [DONE]\n\n",
	}
	firstChunkFlushed := make(chan struct{})
	proceedToChunk2 := make(chan struct{})

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)

		flusher, _ := w.(http.Flusher)
		w.Write([]byte(chunks[0]))
		flusher.Flush()
		close(firstChunkFlushed)

		<-proceedToChunk2

		for _, chunk := range chunks[1:] {
			w.Write([]byte(chunk))
			flusher.Flush()
		}
	}))
	defer upstream.Close()

	// Simulate the handler's fast path: SendStream → HandleStreamResponse → PipeStream
	originalReq := httptest.NewRequest(http.MethodPost, "/test", strings.NewReader("{}"))
	resp, err := doHTTPRequestStream(upstream.Client(), upstream.URL, []byte(`{}`),
		config.Upstream{Token: "test"}, respFormat, originalReq, "")
	if err != nil {
		t.Fatalf("[%s] doHTTPRequestStream error: %v", label, err)
	}

	// HandleStreamResponse: should return streamResp for PipeStream
	status, errBody, _, streamResp, handleErr := HandleStreamResponse(resp)
	if handleErr != nil {
		t.Fatalf("[%s] HandleStreamResponse error: %v", label, handleErr)
	}
	if status != http.StatusOK {
		t.Fatalf("[%s] status = %d, want 200", label, status)
	}
	if errBody != nil {
		t.Fatalf("[%s] errBody should be nil", label)
	}
	if streamResp == nil {
		t.Fatalf("[%s] streamResp should not be nil", label)
	}
	defer streamResp.Body.Close()

	// Pipe to synchronized recorder in background, verify incremental delivery.
	rec := newSyncResponseWriter()
	pipeDone := make(chan PipeResult, 1)
	go func() {
		pipeDone <- PipeStream(rec, streamResp.Body)
	}()

	// Wait for upstream to flush chunk 1
	<-firstChunkFlushed

	// Wait until downstream receives the first write.
	select {
	case <-rec.FirstWrite():
	case <-time.After(1 * time.Second):
		t.Fatalf("[%s] timeout waiting for first chunk at downstream", label)
	}

	// Verify chunk 1 arrived while upstream is still blocked on proceedToChunk2
	body := rec.BodyString()
	if !strings.Contains(body, "chunk1") {
		t.Fatalf("[%s] first chunk not received while upstream blocked, got %q", label, body)
	}

	// Unblock upstream
	close(proceedToChunk2)

	// Wait for PipeStream to complete
	result := <-pipeDone
	if !result.OK() {
		t.Fatalf("[%s] PipeStream failed: upstream=%v downstream=%v", label, result.UpstreamErr, result.DownstreamErr)
	}

	// Verify all data arrived
	allData := rec.BodyString()
	expected := strings.Join(chunks, "")
	if allData != expected {
		t.Fatalf("[%s] body = %q, want %q", label, allData, expected)
	}
}

func TestStreamingFastPath_Codex(t *testing.T) {
	// Responses → Codex: response format is APITypeResponses
	testStreamingFastPathE2E(t, "Responses→Codex", config.APITypeResponses)
}

func TestStreamingFastPath_GeminiCLI(t *testing.T) {
	// Gemini → GeminiCLI: response format is APITypeGemini
	testStreamingFastPathE2E(t, "Gemini→GeminiCLI", config.APITypeGemini)
}

func TestStreamingFastPath_Antigravity(t *testing.T) {
	// Gemini → Antigravity: response format is APITypeGemini
	testStreamingFastPathE2E(t, "Gemini→Antigravity", config.APITypeGemini)
}

func TestStreamingFastPath_ClaudeCode(t *testing.T) {
	// Anthropic → ClaudeCode: response format is APITypeAnthropic
	testStreamingFastPathE2E(t, "Anthropic→ClaudeCode", config.APITypeAnthropic)
}

// ── Non-target regression test ────────────────────────────────────────

func TestStreamingFastPath_NonTargetRemainBuffered(t *testing.T) {
	// Verify that non-target senders (OpenAI, Anthropic, Gemini, Responses)
	// do NOT implement StreamCapableSender, so their streaming paths remain
	// buffered through sender.Send(). This is a regression guard.
	nonTargetTypes := []config.APIType{
		config.APITypeOpenAI,
		config.APITypeAnthropic,
		config.APITypeGemini,
		config.APITypeResponses,
	}

	for _, apiType := range nonTargetTypes {
		t.Run(string(apiType), func(t *testing.T) {
			sender := GetOutboundSender(apiType)
			if _, ok := sender.(StreamCapableSender); ok {
				t.Fatalf("sender for %s should NOT implement StreamCapableSender (would break buffered conversion)", apiType)
			}
		})
	}
}

// ── Test helpers ──────────────────────────────────────────────────────

// syncResponseWriter is a thread-safe http.ResponseWriter used in tests that
// need to observe partial stream writes while PipeStream is still running.
type syncResponseWriter struct {
	header         http.Header
	mu             sync.Mutex
	body           bytes.Buffer
	firstWrite     chan struct{}
	firstWriteOnce sync.Once
}

func newSyncResponseWriter() *syncResponseWriter {
	return &syncResponseWriter{
		header:     make(http.Header),
		firstWrite: make(chan struct{}),
	}
}

func (w *syncResponseWriter) Header() http.Header {
	return w.header
}

func (w *syncResponseWriter) WriteHeader(int) {}

func (w *syncResponseWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	n, err := w.body.Write(p)
	w.mu.Unlock()
	if n > 0 {
		w.firstWriteOnce.Do(func() { close(w.firstWrite) })
	}
	return n, err
}

func (w *syncResponseWriter) FirstWrite() <-chan struct{} {
	return w.firstWrite
}

func (w *syncResponseWriter) BodyString() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.body.String()
}

// failingReader returns data on first read, then error on next read.
type failingReader struct {
	data     []byte
	err      error
	dataRead bool
}

func (r *failingReader) Read(p []byte) (int, error) {
	if !r.dataRead {
		r.dataRead = true
		n := copy(p, r.data)
		return n, nil
	}
	return 0, r.err
}

// failingWriter returns an error on Write.
type failingWriter struct {
	err error
}

func (w *failingWriter) Write(p []byte) (int, error) {
	return 0, w.err
}

func (w *failingWriter) Header() http.Header {
	return http.Header{}
}

func (w *failingWriter) WriteHeader(int) {}
