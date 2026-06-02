-- apid 请求指标分析 (DuckDB 版)
-- 用法：duckdb < scripts/apid-stats.duckdb.sql
--   或   duckdb -c ".read scripts/apid-stats.duckdb.sql"
-- 只读挂载 SQLite 库，不与在线写入抢锁；DuckDB 默认带表头 box 输出。
-- 对应的 sqlite3 版见同目录 apid-stats.sql。

LOAD sqlite;
ATTACH 'data/apid.db' AS s (TYPE sqlite, READ_ONLY);
USE s;

-- 1. 按模型 × 日期聚合
SELECT '=== 按模型 × 日期 ===' AS section;
SELECT upstream_model,
       time::TIMESTAMPTZ::DATE AS day,
       count(*) AS requests,
       format('{:,}', sum(input_tokens)::BIGINT)  AS total_input,
       format('{:,}', sum(output_tokens)::BIGINT) AS total_output,
       format('{:,}', sum(cached_tokens)::BIGINT) AS total_cached,
       format('{:,}', round(avg(input_tokens))::BIGINT) AS avg_input,
       round(avg(output_tokens))  AS avg_output,
       round(avg(ttft_ms))        AS avg_ttft_ms,
       round(100.0 * sum(cached_tokens) / nullif(sum(input_tokens), 0), 1)        AS cache_pct,
       round(1000.0 * sum(output_tokens) / nullif(sum(duration_ms - ttft_ms), 0), 1) AS avg_tok_per_sec
FROM requests
GROUP BY upstream_model, day
ORDER BY upstream_model, day
LIMIT 20;

-- 2. 最近 20 条请求明细
SELECT '=== 最近 20 条请求 ===' AS section;
SELECT time, ttft_ms, upstream_model,
       stream, upstream_status,
       format('{:,}', total_tokens)  AS total_tokens,
       format('{:,}', cached_tokens) AS cached_tokens,
       format('{:,}', input_tokens)  AS input_tokens,
       format('{:,}', output_tokens) AS output_tokens,
       round(100.0 * cached_tokens / nullif(input_tokens, 0), 1)          AS cache_pct,
       round(1000.0 * output_tokens / nullif(duration_ms - ttft_ms, 0), 1) AS tok_per_sec
FROM requests
ORDER BY time DESC
LIMIT 20;

-- 3. 错误请求
SELECT '=== 错误请求 ===' AS section;
SELECT time, upstream_model, client_status, upstream_status, error
FROM requests
WHERE error IS NOT NULL AND error != ''
ORDER BY time DESC
LIMIT 20;
