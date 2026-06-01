-- apid 请求指标分析
-- 用法：cat scripts/apid-stats.sql | sqlite3 -header -column ./data/apid.db

-- 1. 按模型 × 日期聚合
SELECT '=== 按模型 × 日期 ===' AS '';
SELECT upstream_model,
       date(time) AS day,
       COUNT(*) AS requests,
       printf('%,d', SUM(input_tokens)) AS total_input,
       printf('%,d', SUM(output_tokens)) AS total_output,
       printf('%,d', SUM(cached_tokens)) AS total_cached,
       printf('%,d', ROUND(AVG(input_tokens), 0)) AS avg_input,
       ROUND(AVG(output_tokens), 0) AS avg_output,
       ROUND(AVG(ttft_ms), 0) AS avg_ttft_ms,
       ROUND(100.0 * SUM(cached_tokens) / NULLIF(SUM(input_tokens), 0), 1) AS cache_pct,
       ROUND(1000.0 * SUM(output_tokens) / NULLIF(SUM(duration_ms - ttft_ms), 0), 1) AS avg_tok_per_sec
FROM requests
GROUP BY upstream_model, day
ORDER BY upstream_model, day
LIMIT 20;

-- 2. 最近请求明细
SELECT '=== 最近 20 条请求 ===' AS '';
SELECT time, ttft_ms, upstream_model,
       stream, upstream_status,
       printf('%,d', total_tokens) AS total_tokens,
       printf('%,d', cached_tokens) AS cached_tokens,
       printf('%,d', input_tokens) AS input_tokens,
       printf('%,d', output_tokens) AS output_tokens,
       ROUND(100.0 * cached_tokens / NULLIF(input_tokens, 0), 1) AS cache_pct,
       ROUND(1000.0 * output_tokens / NULLIF(duration_ms - ttft_ms, 0), 1) AS tok_per_sec
FROM requests
ORDER BY time DESC
LIMIT 20;

-- 3. 错误请求
SELECT '=== 错误请求 ===' AS '';
SELECT time, upstream_model, client_status, upstream_status, error
FROM requests
WHERE error IS NOT NULL AND error != ''
ORDER BY time DESC
LIMIT 20;
