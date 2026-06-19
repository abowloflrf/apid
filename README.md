# apid

Local LLM API gateway. Routes by path: same protocol = pure forward, different
protocol = conversion (currently Responses → Chat Completions). Supports OpenAI
Chat / Responses / Anthropic Messages.

```
Client ⇄ Responses ⇄ apid ⇄ Chat Completions ⇄ Upstream    (conversion)
Client ⇄ Chat/Resp ⇄ apid ⇄ Chat/Resp ⇄ Upstream           (forward)
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

## Stats

Set `APID_DB` to a SQLite path to enable async per-request metrics (protocol /
model / token / duration / TTFT). Off by default; zero overhead.

```bash
APID_DB=apid.db go run .
sqlite3 apid.db < scripts/apid-stats.sql
```

Also exposes `GET /stats/daily` (per-day aggregation for Grafana Infinity) and
`GET /healthz`.

## Test

```bash
go test ./...
```
