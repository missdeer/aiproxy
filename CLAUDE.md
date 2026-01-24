# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

aiproxy is an API proxy for Anthropic Claude API. It accepts requests on `/v1/messages`, performs model name mapping, and forwards requests to configured upstream services using weighted round-robin load balancing with automatic failover on 4xx/5xx errors.

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
config/config.go         # YAML config loading, model mapping
balancer/balancer.go     # Weighted round-robin load balancer
proxy/handler.go         # HTTP handler, request forwarding, retry logic
```

**Request flow:**
1. Client sends POST to `/v1/messages`
2. Handler parses request, applies model name mapping from config
3. Balancer selects upstream using weighted round-robin
4. Request forwarded to upstream with appropriate auth token
5. On 4xx/5xx, automatically tries next upstream
6. Response returned to client (supports streaming)

## Configuration

See `config.example.yaml` for full example. Key fields:
- `bind`: Bind address (default `127.0.0.1`, use `0.0.0.0` for all interfaces)
- `listen`: Listen port (default `:8080`)
- `upstreams`: List of upstream services, each with:
  - `base_url`: Upstream API endpoint
  - `token`: Authentication token
  - `weight`: Load balancing weight
  - `model_mappings`: Map client model names to upstream model names
  - `available_models`: List of client model names this upstream supports (optional)
  - `must_stream`: Force streaming to upstream, convert back to JSON for non-stream clients (optional)

## Features

- **Per-upstream model mapping**: Each upstream can map client model names to its own model names
- **Model filtering**: Only route requests to upstreams that support the requested model
- **Circuit breaker**: Upstream marked unavailable after 3 consecutive failures, auto-recovers after 30 minutes
- **Detailed logging**: Logs include prompt preview, model mapping, upstream selection, and error responses
- **Must-stream conversion**: Force streaming requests to upstreams that only support streaming, automatically convert SSE responses back to JSON for non-streaming clients
