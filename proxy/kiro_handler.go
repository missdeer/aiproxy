package proxy

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/missdeer/aiproxy/config"
	"github.com/missdeer/aiproxy/oauthcache"
	"github.com/missdeer/aiproxy/upstreammeta"
)

type kiroEndpoint struct {
	URL       string
	Origin    string
	AmzTarget string
	Name      string
}

func kiroEndpoints(region string) []kiroEndpoint {
	if region == "" {
		region = "us-east-1"
	}
	return []kiroEndpoint{
		{
			URL:       fmt.Sprintf("https://codewhisperer.%s.amazonaws.com/generateAssistantResponse", region),
			Origin:    "AI_EDITOR",
			AmzTarget: "AmazonCodeWhispererStreamingService.GenerateAssistantResponse",
			Name:      "CodeWhisperer",
		},
		{
			URL:       fmt.Sprintf("https://q.%s.amazonaws.com/generateAssistantResponse", region),
			Origin:    "CLI",
			AmzTarget: "AmazonQDeveloperStreamingService.SendMessage",
			Name:      "AmazonQ",
		},
	}
}

// KiroTokenStorage mirrors the on-disk JSON format for Kiro auth files.
type KiroTokenStorage struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ClientID     string `json:"client_id,omitempty"`
	ClientSecret string `json:"client_secret,omitempty"`
	AuthMethod   string `json:"auth_method"`
	Region       string `json:"region"`
	ExpiresAt    string `json:"expires_at,omitempty"`
	MachineId    string `json:"machine_id,omitempty"`
	Disabled     bool   `json:"disabled,omitempty"`
}

var kiroAuthManager = oauthcache.NewManager(oauthcache.Config[KiroTokenStorage]{
	Label:          "[KIRO]",
	Load:           kiroLoadStorage,
	Save:           kiroSaveStorage,
	GetAccessToken: func(s *KiroTokenStorage) string { return s.AccessToken },
	Copy:           func(s *KiroTokenStorage) KiroTokenStorage { return *s },
	Refresh:        kiroRefreshAndUpdate,
	Disable: func(path string, s *KiroTokenStorage) error {
		s.Disabled = true
		return kiroSaveStorage(path, s)
	},
})

func kiroRefreshAndUpdate(client *http.Client, storage *KiroTokenStorage, authFile string) (time.Duration, error) {
	if storage.RefreshToken == "" {
		return 0, fmt.Errorf("refresh_token is empty in %s", authFile)
	}

	region := storage.Region
	if region == "" {
		region = "us-east-1"
	}

	var accessToken, refreshToken string
	var expiresIn int
	var statusCode int
	var err error

	if storage.AuthMethod == "social" {
		accessToken, refreshToken, expiresIn, statusCode, err = kiroRefreshSocial(client, storage.RefreshToken, region)
	} else {
		if storage.ClientID == "" || storage.ClientSecret == "" {
			return 0, fmt.Errorf("client_id/client_secret required for idc auth in %s", authFile)
		}
		accessToken, refreshToken, expiresIn, statusCode, err = kiroRefreshIDC(client, storage.RefreshToken, storage.ClientID, storage.ClientSecret, region)
	}

	if err != nil {
		if statusCode == 401 || statusCode == 403 {
			return 0, &oauthcache.ErrAuthDisabled{AuthFile: authFile, Reason: err.Error()}
		}
		return 0, err
	}

	storage.AccessToken = accessToken
	if refreshToken != "" {
		storage.RefreshToken = refreshToken
	}
	storage.ExpiresAt = time.Now().Add(time.Duration(expiresIn) * time.Second).Format(time.RFC3339)

	log.Printf("[KIRO] Token refreshed for %s (method=%s), expires %s", authFile, storage.AuthMethod, storage.ExpiresAt)
	return time.Duration(expiresIn) * time.Second, nil
}

func kiroRefreshSocial(client *http.Client, refreshToken, region string) (string, string, int, int, error) {
	url := fmt.Sprintf("https://prod.%s.auth.desktop.kiro.dev/refreshToken", region)
	payload, _ := json.Marshal(map[string]string{"refreshToken": refreshToken})

	req, err := http.NewRequest("POST", url, bytes.NewReader(payload))
	if err != nil {
		return "", "", 0, 0, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return "", "", 0, 0, err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return "", "", 0, resp.StatusCode, fmt.Errorf("HTTP %d: %s", resp.StatusCode, body)
	}

	var result struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresIn    int    `json:"expiresIn"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", "", 0, resp.StatusCode, fmt.Errorf("parse response: %w", err)
	}
	return result.AccessToken, result.RefreshToken, result.ExpiresIn, resp.StatusCode, nil
}

func kiroRefreshIDC(client *http.Client, refreshToken, clientID, clientSecret, region string) (string, string, int, int, error) {
	url := fmt.Sprintf("https://oidc.%s.amazonaws.com/token", region)
	payload, _ := json.Marshal(map[string]string{
		"clientId":     clientID,
		"clientSecret": clientSecret,
		"refreshToken": refreshToken,
		"grantType":    "refresh_token",
	})

	req, err := http.NewRequest("POST", url, bytes.NewReader(payload))
	if err != nil {
		return "", "", 0, 0, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return "", "", 0, 0, err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return "", "", 0, resp.StatusCode, fmt.Errorf("HTTP %d: %s", resp.StatusCode, body)
	}

	var result struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresIn    int    `json:"expiresIn"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", "", 0, resp.StatusCode, fmt.Errorf("parse response: %w", err)
	}
	return result.AccessToken, result.RefreshToken, result.ExpiresIn, resp.StatusCode, nil
}

// buildKiroPayload converts canonical (Responses API) format to Kiro's native payload.
func buildKiroPayload(requestBody []byte, ep kiroEndpoint) ([]byte, error) {
	var bodyMap map[string]any
	if err := json.Unmarshal(requestBody, &bodyMap); err != nil {
		return nil, fmt.Errorf("parse request body: %w", err)
	}

	model, _ := bodyMap["model"].(string)
	instructions, _ := bodyMap["instructions"].(string)

	var userContent string
	var history []map[string]any
	var toolResults []map[string]any
	var tools []map[string]any

	if input, ok := bodyMap["input"].([]any); ok {
		normalized := make([]map[string]any, 0, len(input))
		for _, item := range input {
			msg, ok := item.(map[string]any)
			if !ok {
				continue
			}
			role, _ := msg["role"].(string)
			if role != "" {
				normalized = append(normalized, msg)
				continue
			}
			itemType, _ := msg["type"].(string)
			switch itemType {
			case "function_call":
				callID, _ := msg["call_id"].(string)
				if callID == "" {
					callID, _ = msg["id"].(string)
				}
				name, _ := msg["name"].(string)
				arguments, _ := msg["arguments"].(string)
				var inputObj any = map[string]any{}
				if arguments != "" {
					if err := json.Unmarshal([]byte(arguments), &inputObj); err != nil {
						inputObj = map[string]any{}
					}
				}
				normalized = append(normalized, map[string]any{
					"role": "assistant",
					"content": []any{
						map[string]any{
							"type":  "tool_use",
							"id":    callID,
							"name":  name,
							"input": inputObj,
						},
					},
				})
			case "function_call_output":
				callID, _ := msg["call_id"].(string)
				if callID == "" {
					callID, _ = msg["id"].(string)
				}
				outputText := kiroExtractTextContent(msg["output"])
				if outputText == "" {
					if s, ok := msg["output"].(string); ok {
						outputText = s
					} else if msg["output"] != nil {
						if b, err := json.Marshal(msg["output"]); err == nil {
							outputText = string(b)
						}
					}
				}
				normalized = append(normalized, map[string]any{
					"role":        "tool",
					"tool_use_id": callID,
					"content":     outputText,
				})
			}
		}

		lastUserIdx := -1
		for idx, msg := range normalized {
			role, _ := msg["role"].(string)
			if role == "user" {
				lastUserIdx = idx
			}
		}

		for idx, msg := range normalized {
			role, _ := msg["role"].(string)
			switch role {
			case "user":
				content := kiroExtractTextContent(msg["content"])
				if idx == lastUserIdx {
					userContent = content
				} else if content != "" {
					history = append(history, map[string]any{
						"userInputMessage": map[string]any{
							"content": content,
							"modelId": model,
							"origin":  ep.Origin,
						},
					})
				}
			case "assistant":
				histMsg := map[string]any{
					"assistantResponseMessage": map[string]any{
						"content": "",
					},
				}
				if contentArr, ok := msg["content"].([]any); ok {
					var textContent string
					var tus []map[string]any
					for _, c := range contentArr {
						cm, ok := c.(map[string]any)
						if !ok {
							continue
						}
						cType, _ := cm["type"].(string)
						if cType == "output_text" {
							if t, ok := cm["text"].(string); ok {
								textContent += t
							}
						} else if cType == "tool_use" {
							tu := map[string]any{
								"toolUseId": cm["id"],
								"name":      cm["name"],
								"input":     cm["input"],
							}
							tus = append(tus, tu)
						}
					}
					arm := histMsg["assistantResponseMessage"].(map[string]any)
					arm["content"] = textContent
					if len(tus) > 0 {
						arm["toolUses"] = tus
					}
				} else if content, ok := msg["content"].(string); ok {
					histMsg["assistantResponseMessage"].(map[string]any)["content"] = content
				}
				history = append(history, histMsg)
			case "tool":
				toolUseID, _ := msg["tool_use_id"].(string)
				if toolUseID == "" {
					if id, ok := msg["id"].(string); ok {
						toolUseID = id
					}
				}
				if toolUseID == "" {
					if id, ok := msg["tool_call_id"].(string); ok {
						toolUseID = id
					}
				}
				if toolUseID == "" {
					// Kiro requires toolUseId; skip malformed tool results.
					continue
				}
				outputText := kiroExtractTextContent(msg["output"])
				if outputText == "" {
					outputText = kiroExtractTextContent(msg["content"])
				}
				tr := map[string]any{
					"toolUseId": toolUseID,
					"content":   []map[string]any{{"text": outputText}},
					"status":    "SUCCESS",
				}
				toolResults = append(toolResults, tr)
			}
		}
	}

	if inputStr, ok := bodyMap["input"].(string); ok {
		userContent = inputStr
	}

	if instructions != "" && userContent != "" {
		userContent = "--- SYSTEM PROMPT ---\n" + instructions + "\n--- END SYSTEM PROMPT ---\n\n" + userContent
	} else if instructions != "" {
		userContent = instructions
	}

	if rawTools, ok := bodyMap["tools"].([]any); ok {
		for _, t := range rawTools {
			tm, ok := t.(map[string]any)
			if !ok {
				continue
			}
			toolType, _ := tm["type"].(string)
			if toolType != "function" {
				continue
			}
			fn, ok := tm["function"].(map[string]any)
			if !ok {
				if name, ok := tm["name"].(string); ok {
					fn = map[string]any{
						"name":        name,
						"description": tm["description"],
						"parameters":  tm["parameters"],
					}
				} else {
					continue
				}
			}
			schema := fn["parameters"]
			if schema != nil {
				if sm, ok := schema.(map[string]any); ok {
					cleanKiroToolSchema(sm)
				}
			}
			tool := map[string]any{
				"toolSpecification": map[string]any{
					"name":        fn["name"],
					"description": fn["description"],
					"inputSchema": map[string]any{
						"json": schema,
					},
				},
			}
			tools = append(tools, tool)
		}
	}

	convID := kiroConversationID(model, instructions, userContent)

	payload := map[string]any{
		"conversationState": map[string]any{
			"chatTriggerType": "MANUAL",
			"conversationId":  convID,
			"currentMessage": map[string]any{
				"userInputMessage": map[string]any{
					"content": userContent,
					"modelId": model,
					"origin":  ep.Origin,
				},
			},
		},
	}

	cs := payload["conversationState"].(map[string]any)
	if len(history) > 0 {
		cs["history"] = history
	}

	currentMsg := cs["currentMessage"].(map[string]any)
	userInput := currentMsg["userInputMessage"].(map[string]any)

	if len(tools) > 0 || len(toolResults) > 0 {
		ctx := map[string]any{}
		if len(tools) > 0 {
			ctx["tools"] = tools
		}
		if len(toolResults) > 0 {
			ctx["toolResults"] = toolResults
		}
		userInput["userInputMessageContext"] = ctx
	}

	maxTokens := 8096
	if mt, ok := bodyMap["max_output_tokens"].(float64); ok && mt > 0 {
		maxTokens = int(mt)
	}
	temp := 1.0
	if t, ok := bodyMap["temperature"].(float64); ok {
		temp = t
	}
	payload["inferenceConfig"] = map[string]any{
		"maxTokens":   maxTokens,
		"temperature": temp,
	}

	return json.Marshal(payload)
}

func kiroExtractTextContent(raw any) string {
	if s, ok := raw.(string); ok {
		return s
	}
	if arr, ok := raw.([]any); ok {
		var b strings.Builder
		for _, c := range arr {
			cm, ok := c.(map[string]any)
			if !ok {
				continue
			}
			if t, ok := cm["text"].(string); ok {
				b.WriteString(t)
			}
		}
		return b.String()
	}
	return ""
}

func kiroConversationID(model, system, firstUser string) string {
	h := sha1.New()
	h.Write([]byte(model))
	h.Write([]byte(system))
	h.Write([]byte(firstUser))
	hash := h.Sum(nil)
	u, _ := uuid.FromBytes(hash[:16])
	return u.String()
}

func cleanKiroToolSchema(schema map[string]any) {
	for k, v := range schema {
		if v == nil {
			delete(schema, k)
			continue
		}
		if k == "additionalProperties" {
			delete(schema, k)
			continue
		}
		if k == "required" {
			if arr, ok := v.([]any); ok && len(arr) == 0 {
				delete(schema, k)
			}
			continue
		}
		if nested, ok := v.(map[string]any); ok {
			cleanKiroToolSchema(nested)
		}
		if arr, ok := v.([]any); ok {
			for _, item := range arr {
				if m, ok := item.(map[string]any); ok {
					cleanKiroToolSchema(m)
				}
			}
		}
	}
}

func normalizeChunk(chunk string, previous *string) string {
	if chunk == "" {
		return ""
	}
	prev := *previous
	if prev == "" {
		*previous = chunk
		return chunk
	}
	if chunk == prev {
		return ""
	}
	if strings.HasPrefix(chunk, prev) {
		delta := chunk[len(prev):]
		*previous = chunk
		return delta
	}
	if strings.HasPrefix(prev, chunk) {
		return ""
	}

	// Handle partial overlap between the suffix of prev and prefix of chunk.
	maxOverlap := 0
	maxLen := len(prev)
	if len(chunk) < maxLen {
		maxLen = len(chunk)
	}
	for i := maxLen; i > 0; i-- {
		if strings.HasSuffix(prev, chunk[:i]) {
			maxOverlap = i
			break
		}
	}

	*previous = chunk
	if maxOverlap > 0 {
		return chunk[maxOverlap:]
	}
	return chunk
}

// ForwardToKiro sends a request to the Kiro upstream with auth retry and dual-endpoint fallback.
// Returns Anthropic-format JSON.
func ForwardToKiro(client *http.Client, upstream config.Upstream, requestBody []byte, clientWantsStream bool) (int, []byte, http.Header, error) {
	if clientWantsStream {
		resp, err := ForwardToKiroStream(client, upstream, requestBody, context.Background())
		if err != nil {
			return 0, nil, nil, err
		}
		status, errBody, headers, streamResp, err := HandleStreamResponse(resp)
		if err != nil {
			return 0, nil, nil, err
		}
		if streamResp == nil {
			return status, errBody, headers, nil
		}
		defer streamResp.Body.Close()
		streamBody, err := io.ReadAll(streamResp.Body)
		if err != nil {
			return 0, nil, nil, fmt.Errorf("read streaming response body: %w", err)
		}
		h := streamResp.Header.Clone()
		StripHopByHopHeaders(h)
		h.Del("Content-Length")
		h.Del("Content-Encoding")
		return status, streamBody, h, nil
	}

	attempts, err := iterAuthFiles(upstream)
	if err != nil {
		return 0, nil, nil, err
	}

	timeout := ClientResponseHeaderTimeout(client)
	var lastErr error
	var lastStatus int
	var lastBody []byte
	var lastHeaders http.Header

	for i, attempt := range attempts {
		token, storage, err := kiroAuthManager.GetToken(attempt.AuthFile, timeout)
		if err != nil {
			log.Printf("[KIRO] Auth file %s (%d/%d): %v", attempt.AuthFile, i+1, len(attempts), err)
			lastErr = err
			continue
		}

		region := storage.Region
		if region == "" {
			region = "us-east-1"
		}
		endpoints := kiroEndpoints(region)

		var endpointErr error
		for _, ep := range endpoints {
			body, buildErr := buildKiroPayload(requestBody, ep)
			if buildErr != nil {
				endpointErr = buildErr
				break
			}

			req, buildErr := buildKiroRequest(upstream, body, token, storage, ep)
			if buildErr != nil {
				endpointErr = buildErr
				break
			}

			resp, doErr := client.Do(req)
			if doErr != nil {
				log.Printf("[KIRO] Endpoint %s failed: %v", ep.Name, doErr)
				endpointErr = doErr
				continue
			}

			if resp.StatusCode == 429 {
				respBody, _ := io.ReadAll(resp.Body)
				resp.Body.Close()
				lastStatus = resp.StatusCode
				lastBody = respBody
				h := resp.Header.Clone()
				StripHopByHopHeaders(h)
				lastHeaders = h
				log.Printf("[KIRO] Endpoint %s returned 429, trying next", ep.Name)
				endpointErr = fmt.Errorf("HTTP %d on %s", resp.StatusCode, ep.Name)
				continue
			}

			if resp.StatusCode == 401 || resp.StatusCode == 403 {
				respBody, _ := io.ReadAll(resp.Body)
				resp.Body.Close()
				log.Printf("[KIRO] Auth file %s (%d/%d): HTTP %d from %s", attempt.AuthFile, i+1, len(attempts), resp.StatusCode, ep.Name)
				lastStatus = resp.StatusCode
				lastBody = respBody
				h := resp.Header.Clone()
				StripHopByHopHeaders(h)
				lastHeaders = h
				endpointErr = fmt.Errorf("HTTP %d: %s", resp.StatusCode, respBody)
				break // try next auth file
			}

			if resp.StatusCode >= 400 {
				respBody, _ := io.ReadAll(resp.Body)
				resp.Body.Close()
				log.Printf("[KIRO] Auth file %s (%d/%d): HTTP %d from %s", attempt.AuthFile, i+1, len(attempts), resp.StatusCode, ep.Name)
				lastStatus = resp.StatusCode
				lastBody = respBody
				h := resp.Header.Clone()
				StripHopByHopHeaders(h)
				lastHeaders = h
				endpointErr = fmt.Errorf("HTTP %d", resp.StatusCode)
				break // try next auth file
			}

			result, parseErr := kiroParseFullResponse(resp.Body)
			resp.Body.Close()
			if parseErr != nil {
				return 0, nil, nil, parseErr
			}

			h := make(http.Header)
			h.Set("Content-Type", "application/json")
			return 200, result, h, nil
		}

		if endpointErr != nil {
			lastErr = endpointErr
		}
	}

	if lastBody != nil {
		return lastStatus, lastBody, lastHeaders, nil
	}
	return lastStatus, nil, nil, lastErr
}

// ForwardToKiroStream sends a streaming request and returns *http.Response
// with body replaced by kiroEventStreamToSSEReader.
func ForwardToKiroStream(client *http.Client, upstream config.Upstream, requestBody []byte, ctx context.Context) (*http.Response, error) {
	attempts, err := iterAuthFiles(upstream)
	if err != nil {
		return nil, err
	}

	timeout := ClientResponseHeaderTimeout(client)
	var lastErr error
	var lastResp *http.Response

	for i, attempt := range attempts {
		token, storage, err := kiroAuthManager.GetToken(attempt.AuthFile, timeout)
		if err != nil {
			log.Printf("[KIRO] Auth file %s (%d/%d): %v", attempt.AuthFile, i+1, len(attempts), err)
			lastErr = err
			continue
		}

		region := storage.Region
		if region == "" {
			region = "us-east-1"
		}
		endpoints := kiroEndpoints(region)

		var endpointErr error
		for _, ep := range endpoints {
			body, buildErr := buildKiroPayload(requestBody, ep)
			if buildErr != nil {
				endpointErr = buildErr
				break
			}

			req, buildErr := buildKiroRequest(upstream, body, token, storage, ep)
			if buildErr != nil {
				endpointErr = buildErr
				break
			}
			req = req.WithContext(ctx)

			resp, doErr := client.Do(req)
			if doErr != nil {
				log.Printf("[KIRO] Endpoint %s failed: %v", ep.Name, doErr)
				endpointErr = doErr
				continue
			}

			if resp.StatusCode == 429 {
				if lastResp != nil {
					lastResp.Body.Close()
				}
				lastResp = resp
				log.Printf("[KIRO] Endpoint %s returned 429, trying next", ep.Name)
				endpointErr = fmt.Errorf("HTTP %d on %s", resp.StatusCode, ep.Name)
				continue
			}

			if resp.StatusCode == 401 || resp.StatusCode == 403 {
				log.Printf("[KIRO] Auth file %s (%d/%d): HTTP %d from %s", attempt.AuthFile, i+1, len(attempts), resp.StatusCode, ep.Name)
				if lastResp != nil {
					lastResp.Body.Close()
				}
				lastResp = resp
				endpointErr = fmt.Errorf("HTTP %d", resp.StatusCode)
				break // try next auth file
			}

			if resp.StatusCode >= 400 {
				log.Printf("[KIRO] Auth file %s (%d/%d): HTTP %d from %s", attempt.AuthFile, i+1, len(attempts), resp.StatusCode, ep.Name)
				if lastResp != nil {
					lastResp.Body.Close()
				}
				lastResp = resp
				endpointErr = fmt.Errorf("HTTP %d", resp.StatusCode)
				break
			}

			if lastResp != nil {
				lastResp.Body.Close()
				lastResp = nil
			}

			// Wrap the body with SSE adapter
			sseReader := newKiroEventStreamToSSEReader(resp.Body)
			resp.Body = sseReader
			resp.Header.Set("Content-Type", "text/event-stream")
			resp.Header.Del("Content-Length")
			return resp, nil
		}

		if endpointErr != nil {
			lastErr = endpointErr
		}
	}

	if lastResp != nil {
		return lastResp, nil
	}
	return nil, lastErr
}

func buildKiroRequest(upstream config.Upstream, body []byte, accessToken string, storage *KiroTokenStorage, ep kiroEndpoint) (*http.Request, error) {
	req, err := http.NewRequest("POST", ep.URL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("X-Amz-Target", ep.AmzTarget)
	req.Header.Set("User-Agent", upstreammeta.KiroUserAgent(storage.MachineId))
	req.Header.Set("X-Amz-User-Agent", upstreammeta.KiroAmzUserAgent(storage.MachineId))
	req.Header.Set("x-amzn-kiro-agent-mode", "vibe")
	req.Header.Set("x-amzn-codewhisperer-optout", "true")
	req.Header.Set("Amz-Sdk-Request", "attempt=1; max=3")
	req.Header.Set("Amz-Sdk-Invocation-Id", uuid.New().String())
	ApplyAcceptEncoding(req, upstream)
	if _, err := ApplyBodyCompression(req, body, upstream); err != nil {
		return nil, fmt.Errorf("request body compression: %w", err)
	}
	return req, nil
}

// kiroParseFullResponse reads the complete Event Stream and returns Anthropic JSON.
func kiroParseFullResponse(body io.Reader) ([]byte, error) {
	var textContent strings.Builder
	var thinkingContent strings.Builder
	var toolUses []map[string]any
	var lastAssistantContent string
	var lastReasoningContent string
	var currentToolUse *kiroToolUseState
	var inputTokens, outputTokens int

	for {
		frame, err := readEventStreamFrame(body)
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read event stream: %w", err)
		}

		if frame.MessageType == "error" || frame.MessageType == "exception" {
			return nil, fmt.Errorf("kiro stream %s: %s", frame.MessageType, frame.Payload)
		}

		if len(frame.Payload) == 0 {
			continue
		}

		var event map[string]any
		if json.Unmarshal(frame.Payload, &event) != nil {
			continue
		}

		inputTokens, outputTokens = kiroUpdateTokens(event, inputTokens, outputTokens)

		switch frame.EventType {
		case "assistantResponseEvent":
			if content, ok := event["content"].(string); ok && content != "" {
				delta := normalizeChunk(content, &lastAssistantContent)
				if delta != "" {
					textContent.WriteString(delta)
				}
			}
		case "reasoningContentEvent":
			if text, ok := event["text"].(string); ok && text != "" {
				delta := normalizeChunk(text, &lastReasoningContent)
				if delta != "" {
					thinkingContent.WriteString(delta)
				}
			}
		case "toolUseEvent":
			currentToolUse = kiroHandleToolUseEvent(event, currentToolUse, &toolUses)
		}
	}

	if currentToolUse != nil {
		kiroFinishToolUse(currentToolUse, &toolUses)
	}

	content := make([]any, 0)
	if thinkingContent.Len() > 0 {
		content = append(content, map[string]any{
			"type":     "thinking",
			"thinking": thinkingContent.String(),
		})
	}
	if textContent.Len() > 0 {
		content = append(content, map[string]any{
			"type": "text",
			"text": textContent.String(),
		})
	}
	for _, tu := range toolUses {
		content = append(content, tu)
	}

	resp := map[string]any{
		"id":          "msg_kiro_" + uuid.New().String()[:8],
		"type":        "message",
		"role":        "assistant",
		"content":     content,
		"model":       "kiro",
		"stop_reason": "end_turn",
		"usage": map[string]any{
			"input_tokens":  inputTokens,
			"output_tokens": outputTokens,
		},
	}

	return json.Marshal(resp)
}

type kiroToolUseState struct {
	ToolUseID   string
	Name        string
	InputBuffer strings.Builder
}

func kiroHandleToolUseEvent(event map[string]any, current *kiroToolUseState, toolUses *[]map[string]any) *kiroToolUseState {
	toolUseID, _ := event["toolUseId"].(string)
	name, _ := event["name"].(string)
	isStop, _ := event["stop"].(bool)

	if toolUseID != "" && name != "" {
		if current == nil {
			current = &kiroToolUseState{ToolUseID: toolUseID, Name: name}
		} else if current.ToolUseID != toolUseID {
			kiroFinishToolUse(current, toolUses)
			current = &kiroToolUseState{ToolUseID: toolUseID, Name: name}
		}
	}

	if current != nil {
		if input, ok := event["input"].(string); ok {
			current.InputBuffer.WriteString(input)
		} else if inputObj, ok := event["input"].(map[string]any); ok {
			data, _ := json.Marshal(inputObj)
			current.InputBuffer.Reset()
			current.InputBuffer.Write(data)
		}
	}

	if isStop && current != nil {
		kiroFinishToolUse(current, toolUses)
		return nil
	}
	return current
}

func kiroFinishToolUse(state *kiroToolUseState, toolUses *[]map[string]any) {
	var input any
	if state.InputBuffer.Len() > 0 {
		var m map[string]any
		if json.Unmarshal([]byte(state.InputBuffer.String()), &m) == nil {
			input = m
		} else {
			input = map[string]any{}
		}
	} else {
		input = map[string]any{}
	}

	*toolUses = append(*toolUses, map[string]any{
		"type":  "tool_use",
		"id":    state.ToolUseID,
		"name":  state.Name,
		"input": input,
	})
}

func kiroUpdateTokens(event map[string]any, currentInput, currentOutput int) (int, int) {
	checkMap := func(m map[string]any) {
		if v, ok := m["inputTokens"].(float64); ok {
			currentInput = int(v)
		}
		if v, ok := m["outputTokens"].(float64); ok {
			currentOutput = int(v)
		}
		if v, ok := m["totalInputTokens"].(float64); ok {
			currentInput = int(v)
		}
		if v, ok := m["totalOutputTokens"].(float64); ok {
			currentOutput = int(v)
		}
	}

	checkMap(event)
	if usage, ok := event["usage"].(map[string]any); ok {
		checkMap(usage)
	}
	if usage, ok := event["tokenUsage"].(map[string]any); ok {
		checkMap(usage)
	}

	return currentInput, currentOutput
}

// kiroEventStreamToSSEReader converts AWS Event Stream binary to Anthropic SSE text.
type kiroEventStreamToSSEReader struct {
	src           io.ReadCloser
	buf           bytes.Buffer
	lastContent   string
	lastReasoning string
	blockIndex    int
	started       bool
	inTextBlock   bool
	inThinkBlock  bool
	currentTool   *kiroToolUseState
	done          bool
	inputTokens   int
	outputTokens  int
}

func newKiroEventStreamToSSEReader(src io.ReadCloser) *kiroEventStreamToSSEReader {
	return &kiroEventStreamToSSEReader{src: src}
}

func (r *kiroEventStreamToSSEReader) Read(p []byte) (int, error) {
	for r.buf.Len() == 0 {
		if r.done {
			return 0, io.EOF
		}
		if err := r.processNextFrame(); err != nil {
			if err == io.EOF {
				r.emitStreamEnd()
				r.done = true
				if r.buf.Len() > 0 {
					break
				}
				return 0, io.EOF
			}
			return 0, err
		}
	}
	return r.buf.Read(p)
}

func (r *kiroEventStreamToSSEReader) Close() error {
	return r.src.Close()
}

func (r *kiroEventStreamToSSEReader) processNextFrame() error {
	frame, err := readEventStreamFrame(r.src)
	if err != nil {
		return err
	}

	if frame.MessageType == "error" || frame.MessageType == "exception" {
		return fmt.Errorf("kiro stream %s: %s", frame.MessageType, frame.Payload)
	}

	if len(frame.Payload) == 0 {
		return nil
	}

	var event map[string]any
	if json.Unmarshal(frame.Payload, &event) != nil {
		return nil
	}

	r.inputTokens, r.outputTokens = kiroUpdateTokens(event, r.inputTokens, r.outputTokens)

	if !r.started {
		r.emitMessageStart()
		r.started = true
	}

	switch frame.EventType {
	case "assistantResponseEvent":
		if content, ok := event["content"].(string); ok && content != "" {
			delta := normalizeChunk(content, &r.lastContent)
			if delta != "" {
				if !r.inTextBlock {
					r.closeCurrentBlock()
					r.emitContentBlockStart("text", "")
					r.inTextBlock = true
					r.inThinkBlock = false
				}
				r.emitTextDelta(delta)
			}
		}
	case "reasoningContentEvent":
		if text, ok := event["text"].(string); ok && text != "" {
			delta := normalizeChunk(text, &r.lastReasoning)
			if delta != "" {
				if !r.inThinkBlock {
					r.closeCurrentBlock()
					r.emitContentBlockStart("thinking", "")
					r.inThinkBlock = true
					r.inTextBlock = false
				}
				r.emitThinkingDelta(delta)
			}
		}
	case "toolUseEvent":
		toolUseID, _ := event["toolUseId"].(string)
		name, _ := event["name"].(string)
		isStop, _ := event["stop"].(bool)

		if toolUseID != "" && name != "" && (r.currentTool == nil || r.currentTool.ToolUseID != toolUseID) {
			r.closeCurrentBlock()
			r.inTextBlock = false
			r.inThinkBlock = false
			r.currentTool = &kiroToolUseState{ToolUseID: toolUseID, Name: name}
			r.emitToolUseStart(toolUseID, name)
		}

		if r.currentTool != nil {
			if input, ok := event["input"].(string); ok {
				r.currentTool.InputBuffer.WriteString(input)
				r.emitInputJSONDelta(input)
			} else if inputObj, ok := event["input"].(map[string]any); ok {
				data, _ := json.Marshal(inputObj)
				r.emitInputJSONDelta(string(data))
			}
		}

		if isStop && r.currentTool != nil {
			r.emitContentBlockStop()
			r.blockIndex++
			r.currentTool = nil
		}
	}

	return nil
}

func (r *kiroEventStreamToSSEReader) closeCurrentBlock() {
	if r.inTextBlock || r.inThinkBlock {
		r.emitContentBlockStop()
		r.blockIndex++
		r.inTextBlock = false
		r.inThinkBlock = false
	}
	if r.currentTool != nil {
		r.emitContentBlockStop()
		r.blockIndex++
		r.currentTool = nil
	}
}

func (r *kiroEventStreamToSSEReader) emitSSE(eventType string, data any) {
	jsonData, _ := json.Marshal(data)
	fmt.Fprintf(&r.buf, "event: %s\ndata: %s\n\n", eventType, jsonData)
}

func (r *kiroEventStreamToSSEReader) emitMessageStart() {
	r.emitSSE("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id":          "msg_kiro_" + uuid.New().String()[:8],
			"type":        "message",
			"role":        "assistant",
			"content":     []any{},
			"model":       "kiro",
			"stop_reason": nil,
			"usage":       map[string]any{"input_tokens": 0, "output_tokens": 0},
		},
	})
}

func (r *kiroEventStreamToSSEReader) emitContentBlockStart(blockType, text string) {
	block := map[string]any{"type": blockType}
	switch blockType {
	case "text":
		block["text"] = ""
	case "thinking":
		block["thinking"] = ""
	}
	r.emitSSE("content_block_start", map[string]any{
		"type":          "content_block_start",
		"index":         r.blockIndex,
		"content_block": block,
	})
}

func (r *kiroEventStreamToSSEReader) emitTextDelta(text string) {
	r.emitSSE("content_block_delta", map[string]any{
		"type":  "content_block_delta",
		"index": r.blockIndex,
		"delta": map[string]any{"type": "text_delta", "text": text},
	})
}

func (r *kiroEventStreamToSSEReader) emitThinkingDelta(thinking string) {
	r.emitSSE("content_block_delta", map[string]any{
		"type":  "content_block_delta",
		"index": r.blockIndex,
		"delta": map[string]any{"type": "thinking_delta", "thinking": thinking},
	})
}

func (r *kiroEventStreamToSSEReader) emitToolUseStart(id, name string) {
	r.emitSSE("content_block_start", map[string]any{
		"type":  "content_block_start",
		"index": r.blockIndex,
		"content_block": map[string]any{
			"type":  "tool_use",
			"id":    id,
			"name":  name,
			"input": map[string]any{},
		},
	})
}

func (r *kiroEventStreamToSSEReader) emitInputJSONDelta(partialJSON string) {
	r.emitSSE("content_block_delta", map[string]any{
		"type":  "content_block_delta",
		"index": r.blockIndex,
		"delta": map[string]any{"type": "input_json_delta", "partial_json": partialJSON},
	})
}

func (r *kiroEventStreamToSSEReader) emitContentBlockStop() {
	r.emitSSE("content_block_stop", map[string]any{
		"type":  "content_block_stop",
		"index": r.blockIndex,
	})
}

func (r *kiroEventStreamToSSEReader) emitStreamEnd() {
	if !r.started {
		r.emitMessageStart()
		r.started = true
	}
	r.closeCurrentBlock()
	r.emitSSE("message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": "end_turn"},
		"usage": map[string]any{"output_tokens": r.outputTokens},
	})
	r.emitSSE("message_stop", map[string]any{
		"type": "message_stop",
	})
}

func kiroLoadStorage(path string) (*KiroTokenStorage, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var s KiroTokenStorage
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, err
	}
	if s.Disabled {
		return nil, &oauthcache.ErrAuthDisabled{AuthFile: path, Reason: "marked disabled on disk"}
	}
	if s.MachineId == "" {
		s.MachineId = uuid.New().String()
		if err := kiroSaveStorage(path, &s); err != nil {
			log.Printf("[KIRO] Failed to persist machine_id to %s: %v", path, err)
		}
	}
	return &s, nil
}

func kiroSaveStorage(path string, s *KiroTokenStorage) error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(path, data, 0644)
}
