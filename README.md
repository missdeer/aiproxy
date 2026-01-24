# aiproxy

API proxy for Anthropic Claude API with intelligent load balancing and automatic failover.

## Features

- **Weighted Round-Robin Load Balancing** - Distribute requests across multiple upstream services based on configured weights
- **Per-Upstream Model Mapping** - Each upstream can map client model names to its own model names
- **Model Filtering** - Only route requests to upstreams that support the requested model
- **Circuit Breaker** - Automatically mark failing upstreams as unavailable (3 consecutive failures), with auto-recovery after 30 minutes
- **Automatic Failover** - Seamlessly retry failed requests on alternative upstreams
- **Detailed Logging** - Track prompt previews, model mappings, upstream selection, and error responses
- **Streaming Support** - Full support for streaming responses
- **Upstream Must-Stream Fallback** - Force streaming-only upstreams while keeping non-stream client compatibility

## Installation

```bash
go build -o aiproxy
```

## Configuration

Create a `config.yaml` file (see `config.example.yaml` for reference):

```yaml
# Bind address (default: 127.0.0.1)
bind: "127.0.0.1"

# Listen port
listen: ":8080"

# Upstream services
upstreams:
  - name: "primary"
    base_url: "https://api.anthropic.com"
    token: "sk-ant-xxx"
    weight: 10
    # Optional: force upstream streaming and convert back to JSON for non-stream clients
    must_stream: false
    model_mappings:
      "claude-3-opus": "claude-3-opus-20240229"
      "claude-3-sonnet": "claude-3-sonnet-20240229"
    available_models:
      - "claude-3-opus"
      - "claude-3-sonnet"

  - name: "backup"
    base_url: "https://backup.example.com"
    token: "sk-yyy"
    weight: 5
    model_mappings:
      "claude-3-opus": "anthropic.claude-3-opus-v1"
    available_models:
      - "claude-3-opus"
```

## Usage

Start the proxy:

```bash
./aiproxy -config config.yaml
```

Send requests to the proxy:

```bash
curl http://localhost:8080/v1/messages \
  -H "Content-Type: application/json" \
  -H "x-api-key: any-key" \
  -H "anthropic-version: 2023-06-01" \
  -d '{
    "model": "claude-3-opus",
    "max_tokens": 1024,
    "messages": [{"role": "user", "content": "Hello!"}]
  }'
```

The proxy will:
1. Map `claude-3-opus` to the appropriate upstream model name
2. Select an available upstream using weighted round-robin
3. Forward the request with the mapped model name
4. Automatically retry on other upstreams if the request fails

## How It Works

1. **Request Reception** - Client sends request with a unified model name
2. **Model Filtering** - Proxy filters upstreams that support the requested model
3. **Load Balancing** - Weighted round-robin selects the next upstream
4. **Model Mapping** - Client model name is mapped to upstream-specific model name
5. **Request Forwarding** - Request is sent to the selected upstream
6. **Automatic Retry** - On 4xx/5xx errors, automatically tries the next upstream
7. **Circuit Breaking** - After 3 consecutive failures, upstream is marked unavailable for 30 minutes

## License

This project is dual-licensed:

- **Personal Use**: GNU General Public License v3.0 (GPL-3.0)
  - Free for personal projects, educational purposes, and non-commercial use

- **Commercial Use**: Commercial License Required
  - For commercial or workplace use, please contact: **missdeer@gmail.com**
  - See [LICENSE-COMMERCIAL](LICENSE-COMMERCIAL) for details

See [LICENSE](LICENSE) for the full GPL-3.0 license text.
