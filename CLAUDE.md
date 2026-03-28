# Handoff — Agent-to-Agent Coordination Relay

## What this is

A hosted relay API for multi-agent coordination. Agents communicate through channels with messages, threading, status tracking, acks, and real-time SSE push. Ships with a TypeScript SDK and MCP server for Claude Code integration.

## Architecture

```
├── server/              # Go server (the actual service)
│   ├── main.go          # Entry point, graceful shutdown, static files
│   ├── store/           # Redis data layer, all business logic
│   │   ├── store.go     # Store struct, all CRUD operations
│   │   └── store_test.go # 22 unit tests
│   ├── handler/         # HTTP handlers, middleware, SSE
│   │   ├── handler.go   # Routes, auth, rate limiting, all endpoints
│   │   └── handler_test.go # 26 integration tests
│   └── testutil/        # Shared test helpers (Redis client, flush)
├── src/                 # TypeScript client code (NOT the server)
│   ├── sdk.ts           # TypeScript SDK (import { Handoff } from "handoff-sdk")
│   ├── mcp.ts           # MCP server for Claude Code
│   └── types.ts         # Shared TypeScript types
├── public/              # Static dashboard files
├── Dockerfile           # Multi-stage Go build → Alpine
└── docker-compose.yml   # Server + Redis, Coolify-compatible
```

## Tech stack

- **Server:** Go 1.26, net/http (stdlib router), go-redis/v9
- **Data:** Redis streams (messages), hashes (status, acks, channels), sets (channel membership)
- **SSE:** `http.Flusher` with 8KB padding for Traefik proxy compatibility
- **Client SDK:** TypeScript, published as `handoff-sdk`
- **MCP server:** TypeScript, `@modelcontextprotocol/sdk`
- **Deploy:** Coolify via Docker Compose, Traefik reverse proxy

## Key design decisions

- **Redis streams** for messages — gives us ordered IDs, cursor-based pagination, and XREAD BLOCK for SSE
- **Team-namespaced keys** — all Redis keys prefixed with `t:{teamID}:` for multi-tenant isolation
- **Threads stored in separate streams** — `thr:{channel}:{parentId}` for fast thread reads, reply_count in a hash
- **XRANGE with `(` prefix** for exclusive cursor reads (not XREAD) to avoid blocking on non-SSE paths
- **8KB SSE padding** on connect to push through Traefik's response buffer
- **Traefik label** `responseforwarding.flushinterval=1ms` in docker-compose for proper SSE streaming

## Running locally

```bash
# Start Redis
docker run -d --name handoff-redis -p 6379:6379 redis:7-alpine

# Run server
cd server && go run .

# Run tests (requires Redis on localhost:6379)
cd server && go test ./... -v
```

## Running tests

Tests require a running Redis instance. By default they connect to `redis://localhost:6379/15` (db 15 to avoid conflicts). Override with `REDIS_TEST_URL` env var.

```bash
docker run -d --name handoff-test-redis -p 6379:6379 redis:7-alpine
cd server && go test ./... -v -count=1
```

**Never skip tests.** If Redis isn't available, install it — don't mark things done with skipped tests.

## Environment variables

| Var | Default | Description |
|-----|---------|-------------|
| `PORT` | 3000 | Server port |
| `REDIS_URL` | redis://localhost:6379 | Redis connection URL |
| `RATE_LIMIT_MAX` | 100 | Max requests per window |
| `RATE_LIMIT_WINDOW_MS` | 1000 | Rate limit window in ms |
| `RELAY_API_URL` | — | MCP server: relay API base URL |
| `RELAY_API_KEY` | — | MCP server: API key |

## API overview

All endpoints return structured JSON errors: `{"error": "message", "code": "ERROR_CODE"}`

**No auth required:** `GET /healthz`, `GET /readyz`, `GET /api`, `POST /api/signup`

**Auth required** (Bearer token, X-Api-Key header, or `?token=` query param):
- Channels: `POST/GET/DELETE /api/channels`
- Messages: `POST/GET/DELETE /api/channels/{ch}/messages` — supports `thread_id`, `mention`, `sender` filters
- Threads: `GET /api/channels/{ch}/threads/{id}`
- SSE: `GET /api/channels/{ch}/stream?token=KEY`
- Acks: `POST/GET /api/channels/{ch}/ack(s)`
- Status: `PUT/GET /api/channels/{ch}/status`, `GET /api/status` (cross-channel)
- Status changes: `GET /api/channels/{ch}/status/changes`
- Audit log: `GET /api/audit` — permission denial log (admin-only, query: `after_id`, `limit`, `result`)

## Input validation

- Channel names: `^[a-zA-Z0-9][a-zA-Z0-9._-]{0,127}$`
- Cursors: `^\d+-\d+$` (Redis stream ID format)
- Message content: max 32KB
- Status keys: max 256 chars
- Status values: max 32KB
- Limit param: 1-100
- Default cursor for empty reads: `0-0` (valid for feed-back)

## Coolify deployment notes

- Deployment type: Docker Compose
- Traefik label `responseforwarding.flushinterval=1ms` is required for SSE — set in docker-compose.yml
- Healthcheck: `curl -f http://localhost:3000/readyz` (30s interval, 10s timeout, 5 retries)
- Redis healthcheck: `redis-cli ping`
- Server waits for Redis healthy before starting (`depends_on: condition: service_healthy`)

## SDK usage

```typescript
import { Handoff } from "handoff-sdk";

const hf = new Handoff({ apiUrl: "https://handoff.xaviair.dev", apiKey: "relay_..." });

await hf.post("deploy", "build started");
await hf.reply("deploy", msgId, "looks good!");
const thread = await hf.thread("deploy", msgId);
const unsub = hf.on("deploy", (msg) => console.log(msg)); // SSE
await hf.setStatus("deploy", "stage", "building");
```
