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

Providers that natively speak both OpenAI protocols only need one upstream:
set `supports_responses = true` on an `openai_chat_completions` upstream (and
optionally `responses_path`). A `openai_responses` route hitting it then
forward raw bytes to the Responses endpoint instead of converting to Chat
Completions.

To protect the gateway itself, set a top-level `client_api_key` in
`config.toml`. When set, clients must send it as `Authorization: Bearer ...`,
`X-Api-Key`, or `Api-Key`. This inbound key is stripped before forwarding; the
upstream credential remains `[[upstream]].api_key` (Anthropic uses `X-Api-Key`).
Only forwarding routes require it; `/healthz` and `/stats/*` stay open.

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
series, per-model breakdown and a request log, all filterable by time range /
model / protocol). It's self-contained — embedded in the binary, no external CDN.

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

Also `GET /healthz`.

## Test

```bash
go test ./...
```
