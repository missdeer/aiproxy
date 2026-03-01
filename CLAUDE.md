# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

aiproxy is an API proxy for AI services. It accepts requests in Anthropic, OpenAI, Responses API, or Gemini format, performs model name mapping, translates between protocols, and forwards requests to configured upstream services using weighted round-robin load balancing with automatic failover on 4xx/5xx errors.

## Build Commands

```bash
go build ./...           # Build
go test ./...            # Run all tests
go test -run TestName ./path/to/package  # Run single test
go fmt ./...             # Format
go vet ./...             # Vet
```

## Running

```bash
./aiproxy -config config.yaml
```

## Architecture

```
main.go                  # Entry point, HTTP server setup
config/config.go         # YAML config loading, model mapping, hot-reload
balancer/balancer.go     # Weighted round-robin load balancer with circuit breaker
proxy/base_handler.go    # Shared handler base, HTTP client factory
proxy/anthropic_handler.go   # Anthropic Messages API handler
proxy/openai_handler.go      # OpenAI Chat Completions handler
proxy/responses_handler.go   # OpenAI Responses API handler
proxy/gemini_handler.go      # Gemini API handler
proxy/outbound.go            # OutboundSender interface for protocol translation
proxy/codex_handler.go       # Codex OAuth upstream (Forward function)
proxy/claudecode_handler.go  # Claude Code OAuth upstream (Forward function)
proxy/geminicli_handler.go   # Gemini CLI OAuth upstream (Forward function)
proxy/antigravity_handler.go # Antigravity OAuth upstream (Forward function)
proxy/kiro_handler.go        # Kiro (AWS CodeWhisperer/AmazonQ) OAuth upstream (Forward function)
proxy/kiro_eventstream.go    # AWS Event Stream binary frame parser with CRC32 validation
oauthcache/                  # Generic OAuth token cache with auto-refresh
upstreammeta/                # Upstream metadata (User-Agent strings)
```

**Request flow:**
1. Client sends POST to `/v1/messages`, `/v1/chat/completions`, `/v1/responses`, or `/v1beta/models/*`
2. Handler parses request, applies model name mapping from config
3. Balancer selects upstream using weighted round-robin (filtered by model support)
4. Request translated to upstream's native protocol format
5. Request forwarded to upstream with appropriate auth (API key or OAuth)
6. On 4xx/5xx, automatically tries next upstream
7. Response translated back to client's expected format
8. Circuit breaker marks upstream unavailable after 3 consecutive failures

## Configuration

See `config.example.yaml` for full example. Key fields:
- `bind`: Bind address (default `127.0.0.1`, use `0.0.0.0` for all interfaces)
- `listen`: Listen port (default `:8080`)
- `upstream_request_timeout`: Seconds to wait for upstream response headers (default `60`)
- `upstreams`: List of upstream services, each with:
  - `base_url`: Upstream API endpoint
  - `token`: Authentication token (API key-based upstreams)
  - `auth_files`: OAuth auth file paths (OAuth-based upstreams, round-robin)
  - `weight`: Load balancing weight
  - `api_type`: Protocol type (`anthropic`, `openai`, `gemini`, `responses`, `codex`, `geminicli`, `antigravity`, `claudecode`, `kiro`)
  - `model_mappings`: Map client model names to upstream model names
  - `available_models`: List of client model names this upstream supports (optional)

## Features

- **Multi-protocol support**: 9 upstream API types with automatic protocol translation
- **Per-upstream model mapping**: Each upstream can map client model names to its own model names
- **Model filtering**: Only route requests to upstreams that support the requested model
- **Circuit breaker**: Upstream marked unavailable after 3 consecutive failures per model, auto-recovers after 30 minutes
- **OAuth authentication**: Support for OAuth-based upstreams with automatic token refresh
- **Auth file round-robin**: Multiple auth files per upstream, rotated in round-robin fashion
- **Streaming support**: Full SSE streaming support across all protocols
- **Config hot-reload**: Config file changes are watched and applied without restart
- **Rotating file logging**: Optional file-based logging with automatic rotation by size/age

## Notice

- Never use `/tmp` to store any files, use relative path instead