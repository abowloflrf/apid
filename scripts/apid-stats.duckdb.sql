-- apid 请求指标分析 (DuckDB 版)
-- 用法：duckdb < scripts/apid-stats.duckdb.sql
--   或   duckdb -c ".read scripts/apid-stats.duckdb.sql"
-- 只读挂载 SQLite 库，不与在线写入抢锁；DuckDB 默认带表头 box 输出。
-- 对应的 sqlite3 版见同目录 apid-stats.sql。
-- 口径：
--   e2e_tok_per_sec: output_tokens / duration_ms，适合看用户实际感知速度。
--   post_ttft_tok_per_sec: output_tokens / (duration_ms - ttft_ms)，仅在 TTFT
--     真正表示首个输出 token 时有意义；Codex 高 reasoning 请求里容易虚高。

LOAD sqlite;
ATTACH 'data/apid.db' AS s (TYPE sqlite, READ_ONLY);
USE s;

-- 1. 按模型总计
SELECT '=== 按模型总计 ===' AS section;
SELECT upstream_model,
       count(*) AS requests,
       format('{:,}', sum(input_tokens)::BIGINT)  AS total_input,
       format('{:,}', sum(output_tokens)::BIGINT) AS total_output,
       format('{:,}', sum(cached_tokens)::BIGINT) AS total_cached,
       round(100.0 * sum(cached_tokens) / nullif(sum(input_tokens), 0), 2) AS cache_pct
FROM requests
GROUP BY upstream_model
ORDER BY requests DESC;

-- 2. 按模型 × 日期聚合
SELECT '=== 按模型 × 日期 ===' AS section;
SELECT upstream_model,
       time::TIMESTAMPTZ::DATE AS day,
       count(*) AS requests,
       format('{:,}', sum(input_tokens)::BIGINT)  AS total_input,
       format('{:,}', sum(output_tokens)::BIGINT) AS total_output,
       format('{:,}', sum(cached_tokens)::BIGINT) AS total_cached,
       format('{:,}', round(avg(input_tokens))::BIGINT) AS avg_input,
       round(avg(output_tokens))  AS avg_output,
       round(avg(duration_ms))    AS avg_duration_ms,
       round(avg(ttft_ms))        AS avg_ttft_ms,
       round(100.0 * sum(cached_tokens) / nullif(sum(input_tokens), 0), 2)        AS cache_pct,
       round(1000.0 * sum(output_tokens) / nullif(sum(duration_ms), 0), 1) AS e2e_tok_per_sec,
       round(1000.0 * sum(output_tokens) / nullif(sum(duration_ms - coalesce(ttft_ms, 0)), 0), 1) AS post_ttft_tok_per_sec
FROM requests
GROUP BY upstream_model, day
ORDER BY day DESC, upstream_model
LIMIT 20;

-- 3. 最近 20 条请求明细
SELECT '=== 最近 20 条请求 ===' AS section;
SELECT time, upstream_model,
       duration_ms, ttft_ms,
       stream, upstream_status,
       format('{:,}', input_tokens)  AS input_len,
       format('{:,}', output_tokens) AS output_len,
       format('{:,}', cached_tokens) AS cache_hit_len,
       CASE WHEN cached_tokens > 0 THEN 'hit' ELSE 'miss' END AS cache_hit,
       round(100.0 * cached_tokens / nullif(input_tokens, 0), 2)          AS cache_pct,
       format('{:,}', total_tokens)  AS total_tokens,
       round(1000.0 * output_tokens / nullif(duration_ms, 0), 1) AS e2e_tok_per_sec,
       round(1000.0 * output_tokens / nullif(duration_ms - coalesce(ttft_ms, 0), 0), 1) AS post_ttft_tok_per_sec,
       client_ua
FROM requests
ORDER BY time DESC
LIMIT 20;

-- 4. 错误请求
SELECT '=== 错误请求 ===' AS section;
SELECT time, upstream_model, client_status, upstream_status, error
FROM requests
WHERE error IS NOT NULL AND error != ''
ORDER BY time DESC
LIMIT 20;
