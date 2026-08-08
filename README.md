# apid

Local LLM API gateway. Routes by path: same protocol = pure forward, different
protocol = conversion (currently Responses → Chat Completions). Supports OpenAI
Chat / Responses / Anthropic Messages.

```
Client ⇄ Responses ⇄ apid ⇄ Chat Completions ⇄ Upstream    (conversion)
Client ⇄ Chat/Resp ⇄ apid ⇄ Chat/Resp ⇄ Upstream           (forward)
Client ⇄ Responses ⇄ apid ⇄ Responses ⇄ Upstream           (dual protocol, supports_responses)
Client ⇄ Messages ⇄ apid ⇄ Messages ⇄ Anthropic            (forward)
```

**Supported**: text, function calling, reasoning. Thinking models (DeepSeek-R1,
Kimi) work via a built-in reasoning-content cache that replays previous-turn
reasoning across multi-turn conversations.
**Not supported**: image/file input, built-in tools (web_search), annotations,
multimodal output, `previous_response_id`.

## Run

```bash
cp config.example.toml config.toml   # edit upstream addresses / protocol / keys / models
go run . --config config.toml        # listens on :19092 by default (override with APID_LISTEN)
```

Config uses `[[upstream]]` (backends, referenced by `name`) and `[[route]]`
(public paths + `model` dispatch rules). Env vars live in `.env` (see
`.env.example`); full syntax in `config.example.toml`.

Graceful shutdown waits up to 120 seconds for in-flight HTTP requests by
default. Set `APID_SHUTDOWN_TIMEOUT` to another positive Go duration, such as
`30s` or `3m`, when the surrounding process supervisor allows at least that
long before forcefully terminating apid.

Providers that natively speak both OpenAI protocols only need one upstream:
set `supports_responses = true` on an `openai_chat_completions` upstream (and
optionally `responses_path`). A `openai_responses` route hitting it then
forward raw bytes to the Responses endpoint instead of converting to Chat
Completions.

To protect the gateway itself, set a top-level `client_api_key` in
`config.toml`. When set, clients must send it as `Authorization: Bearer ...`,
`X-Api-Key`, or `Api-Key`. This inbound key is stripped before forwarding; the
upstream credential remains `[[upstream]].api_key` (Anthropic uses `X-Api-Key`).
Only forwarding routes require it; `/healthz` and `/api/hello` stay open.

## Experimental Codex subscription proxy

`auth_mode = "codex_subscription"` lets a Codex client that is already signed
in with ChatGPT send its OpenAI Bearer credential through apid to the fixed
`https://chatgpt.com/backend-api/codex` backend. This is credential passthrough,
not subscription verification: apid does not parse the token or determine
whether the account is Plus/Pro. The upstream remains responsible for access.

The mode supports only `POST /responses` and `POST /responses/compact`; it does
not implement `/models`. The upstream URL, protocol, model passthrough and route
shape are validated at startup. Subscription routes also disable environment
proxies, redirects, request replay, TRACE bodies and query persistence. Because
the local process can see the Bearer credential, apid requires an IP-literal
loopback listener such as `APID_LISTEN=127.0.0.1:19092`.
The subscription SSE idle timeout defaults to five minutes and can be changed
with `APID_CODEX_SSE_IDLE_TIMEOUT` using a Go duration such as `10m`.

The complete apid-side example is commented in `config.example.toml`. Codex can
point a custom Responses provider at it:

```toml
model_provider = "apid-codex-subscription"

[model_providers.apid-codex-subscription]
name = "OpenAI"
base_url = "http://127.0.0.1:19092/codex/v1"
wire_api = "responses"
requires_openai_auth = true
supports_websockets = false
request_max_retries = 0
stream_max_retries = 0
stream_idle_timeout_ms = 300000

# Only when apid's client_api_key is enabled:
env_http_headers = { "X-Apid-Key" = "APID_CODEX_PROXY_KEY" }
```

Run Codex's ChatGPT login flow before selecting this provider. Compatibility
with the private ChatGPT Codex endpoints can change across Codex versions, so
verify inference, tool calls and context compaction after upgrades. See
[`docs/CODEX_SUBSCRIPTION_PROXY_DESIGN.md`](docs/CODEX_SUBSCRIPTION_PROXY_DESIGN.md)
for the threat model and compatibility limits.

The read-only dashboard and its JSON API (`GET /stats` and `/stats/*`) are
guarded independently by a top-level `stats_api_key`. When set, the same three
header styles are accepted. Leave it empty to keep stats open (Grafana can send
the key as a custom header in its Infinity datasource).

## Session CLI

`apid-ctl` lists local Codex, Claude Code, pi, and OpenCode sessions using the
same Go readers as `GET /stats/sessions`. On a terminal it opens a responsive,
keyboard-navigable table; redirected output automatically uses a plain table.

```bash
go run ./cmd/apid-ctl                 # latest 20 token-bearing sessions
go run ./cmd/apid-ctl --tool codex -n 50
go run ./cmd/apid-ctl --cwd apid --since 7d
go run ./cmd/apid-ctl --json
go install ./cmd/apid-ctl             # install the standalone binary
```

Use `--all` to remove the limit, `--archived` for archived sessions, and
`--plain` to skip the interactive table. The source locations honor
`CODEX_HOME`, `CODEX_SQLITE_HOME`, `CLAUDE_HOME`, `PI_CODING_AGENT_DIR`,
`OPENCODE_DATA_HOME`, and `OPENCODE_DB` just like the Python script.

## Docker

```bash
cp config.example.toml config.toml
# edit upstream addresses / keys in config.toml
docker compose up -d
```

## Usage

```bash
curl http://localhost:19092/v1/responses \
  -H "Content-Type: application/json" \
  -d '{"model":"gpt-4o-mini","input":"Tell me about yourself in one sentence."}'
```

Add `"stream": true` for SSE streaming. `input` accepts strings or message arrays;
`tools` supports function calling.

## Web Search

Optional. When `[search]` is configured (Exa backend), apid serves
`POST /v1/alpha/search` — the endpoint Codex calls when a custom provider opts
into `supports_standalone_web_search`, letting Codex's built-in `web.run` tool
work with non-OpenAI backends.

```toml
[search]
provider = "exa"
api_key  = "your-exa-api-key"
```

Codex side:

```toml
[model_providers.apid]
supports_standalone_web_search = true
```

## Stats

Set `APID_DB` to a SQLite path to enable async per-request metrics (protocol /
model / upstream URL / token / cached tokens / duration / TTFT). Off by default;
zero overhead.

```bash
APID_DB=apid.db go run .
sqlite3 apid.db < scripts/apid-stats.sql
```

With `APID_DB` set, open **`/stats/`** for an interactive dashboard (KPIs, time
series, per-model breakdown and a pageable request log, all filterable by time
range / model / protocol). It's self-contained — embedded in the binary, no
external CDN.

The dashboard is a Vite + React + TypeScript project under `webui/`. Its build
output lands in `server/webui/dist`, which the Go binary embeds:

```bash
pnpm --dir webui build   # needs pnpm; the Docker build does this automatically
go build ./...
```

For frontend development run `pnpm --dir webui dev` (proxies `/stats/*` to a
local apid on :19092). Nothing under `server/webui/dist/` is committed - it's
all build output regenerated by `pnpm --dir webui build`, which must run before
`go build ./...` (the Go side embeds `dist/` via `//go:embed`).

JSON API behind it (shared params `from`/`to` as Unix ms or RFC3339, `tz_offset`,
repeatable/comma-separated `model` & `protocol`):

| Endpoint | Returns |
| --- | --- |
| `GET /stats/summary` | grand-total metrics for the window |
| `GET /stats/by_model` | per-upstream-model aggregation |
| `GET /stats/timeseries` | time buckets (`bucket=15min\|hour\|day`) |
| `GET /stats/requests` | recent request detail (`limit`, `offset`, `errors_only`) |
| `GET /stats/options` | distinct models / protocols + time span (filter UI) |
| `GET /stats/daily` | per-day aggregation for Grafana Infinity |
| `GET /stats/topology` | loaded routes/upstreams graph, secrets redacted (needs no `APID_DB`) |

Also `GET /healthz`, plus unauthenticated `GET`/`HEAD /api/hello` for Claude
Code provider preflight checks. `/api/hello` is answered locally and never
calls an upstream model.

## Test

```bash
go test ./...
```
