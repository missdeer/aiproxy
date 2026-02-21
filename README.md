# aiproxy

API proxy for AI services with intelligent load balancing, automatic failover, and multi-protocol translation.

Accepts requests in **Anthropic**, **OpenAI**, or **Responses API** format, and routes them to any supported upstream — translating between protocols automatically.

## Features

- **Multi-Protocol Support** — 8 upstream API types: Anthropic, OpenAI, Gemini, Responses, Codex, Gemini CLI, Antigravity, Claude Code
- **Automatic Protocol Translation** — Clients speak one API format; the proxy translates to each upstream's native format
- **Weighted Round-Robin Load Balancing** — Distribute requests across multiple upstream services based on configured weights
- **Per-Upstream Model Mapping** — Each upstream can map client model names to its own model names
- **Model Filtering** — Only route requests to upstreams that support the requested model
- **Circuit Breaker** — Automatically mark failing upstreams as unavailable (3 consecutive failures), with auto-recovery after 30 minutes
- **Automatic Failover** — Seamlessly retry failed requests on alternative upstreams
- **OAuth Authentication** — Support for OAuth-based upstreams (Codex, Gemini CLI, Antigravity, Claude Code) with automatic token refresh
- **Auth File Round-Robin** — Multiple auth files per upstream, rotated in round-robin fashion
- **Streaming Support** — Full support for streaming responses across all protocols
- **Upstream Must-Stream Fallback** — Force streaming-only upstreams while keeping non-stream client compatibility
- **Rotating File Logging** — Optional file-based logging with automatic rotation by size/age

## Supported API Types

| API Type | Protocol | Authentication | Endpoint |
|---|---|---|---|
| `anthropic` | Anthropic Messages | API key (`token`) | `/v1/messages` |
| `openai` | OpenAI Chat Completions | API key (`token`) | `/v1/chat/completions` |
| `gemini` | Google Gemini | API key (`token`) | `/v1beta/models/*/generateContent` |
| `responses` | OpenAI Responses | API key (`token`) | `/v1/responses` |
| `codex` | ChatGPT Codex | OAuth (`auth_files`) | `chatgpt.com/backend-api/codex/responses` |
| `geminicli` | Gemini CLI | OAuth (`auth_files`) | `cloudcode-pa.googleapis.com` |
| `antigravity` | Antigravity | OAuth (`auth_files`) | `daily-cloudcode-pa.googleapis.com` |
| `claudecode` | Claude Code | OAuth (`auth_files`) | `api.anthropic.com` |

## Installation

```bash
go build -o aiproxy
```

## Configuration

Create a `config.yaml` file (see `config.example.yaml` for reference):

```yaml
# Server settings
bind: "127.0.0.1"
listen: ":8080"
default_max_tokens: 4096

# Rotating file log (optional, omit for stdout)
log:
  file: "/var/log/aiproxy/aiproxy.log"
  max_size: 100       # MB before rotation
  max_backups: 3      # old files to keep
  max_age: 28         # days to retain
  compress: false     # gzip old files

# Upstream services
upstreams:
  # API key-based upstream
  - name: "primary"
    base_url: "https://api.anthropic.com"
    token: "sk-ant-xxx"
    weight: 10
    api_type: "anthropic"
    must_stream: false
    model_mappings:
      "claude-3-opus": "claude-3-opus-20240229"
      "claude-3-sonnet": "claude-3-sonnet-20240229"
    available_models:
      - "claude-3-opus"
      - "claude-3-sonnet"

  # OAuth-based upstream (Gemini CLI / Antigravity / Codex / Claude Code)
  - name: "Gemini CLI"
    weight: 5
    api_type: "geminicli"
    auth_files:
      - "/path/to/geminicli-auth1.json"
      - "/path/to/geminicli-auth2.json"
    model_mappings:
      "gemini-3-pro": "gemini-3-pro-preview"
    available_models:
      - "gemini-3-pro"
```

## Usage

Start the proxy:

```bash
./aiproxy -config config.yaml
```

The proxy exposes five client-facing endpoints:

```bash
# Anthropic Messages API
curl http://localhost:8080/v1/messages \
  -H "Content-Type: application/json" \
  -H "x-api-key: any-key" \
  -H "anthropic-version: 2023-06-01" \
  -d '{"model": "claude-3-opus", "max_tokens": 1024, "messages": [{"role": "user", "content": "Hello!"}]}'

# OpenAI Chat Completions API
curl http://localhost:8080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer any-key" \
  -d '{"model": "gpt-4o", "messages": [{"role": "user", "content": "Hello!"}]}'

# OpenAI Responses API
curl http://localhost:8080/v1/responses \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer any-key" \
  -d '{"model": "gpt-4o", "input": "Hello!"}'

# Gemini API (compatible with Google AI Studio SDKs)
# /v1beta/models/{model}:generateContent
# /v1/models/{model}:generateContent
curl http://localhost:8080/v1beta/models/gemini-pro:generateContent \
  -H "Content-Type: application/json" \
  -H "x-goog-api-key: any-key" \
  -d '{"contents": [{"parts": [{"text": "Hello!"}]}]}'
```

## How It Works

1. **Request Reception** — Client sends request in any supported format (Anthropic / OpenAI / Responses)
2. **Model Filtering** — Proxy filters upstreams that support the requested model
3. **Load Balancing** — Weighted round-robin selects the next upstream
4. **Protocol Translation** — Request is converted from the client's format to the upstream's native format
5. **Model Mapping** — Client model name is mapped to upstream-specific model name
6. **Must-Stream Handling** — If upstream requires streaming, force `stream=true` and convert response back
7. **Request Forwarding** — Request is sent to the selected upstream
8. **Response Translation** — Upstream response is converted back to the client's expected format
9. **Automatic Retry** — On 4xx/5xx errors, automatically tries the next upstream
10. **Circuit Breaking** — After 3 consecutive failures, upstream is marked unavailable for 30 minutes

## License

This project is dual-licensed:

- **Personal Use**: GNU General Public License v3.0 (GPL-3.0)
  - Free for personal projects, educational purposes, and non-commercial use

- **Commercial Use**: Commercial License Required
  - For commercial or workplace use, please contact: **missdeer@gmail.com**
  - See [LICENSE-COMMERCIAL](LICENSE-COMMERCIAL) for details

See [LICENSE](LICENSE) for the full GPL-3.0 license text.
