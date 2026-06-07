-- apid 请求指标分析
-- 用法：cat scripts/apid-stats.sql | sqlite3 -header -column ./data/apid.db
-- 口径：
--   e2e_tok_per_sec: output_tokens / duration_ms，适合看用户实际感知速度。
--   post_ttft_tok_per_sec: output_tokens / (duration_ms - ttft_ms)，仅在 TTFT
--     真正表示首个输出 token 时有意义；Codex 高 reasoning 请求里容易虚高。

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
       ROUND(AVG(duration_ms), 0) AS avg_duration_ms,
       ROUND(AVG(ttft_ms), 0) AS avg_ttft_ms,
       ROUND(100.0 * SUM(cached_tokens) / NULLIF(SUM(input_tokens), 0), 1) AS cache_pct,
       ROUND(1000.0 * SUM(output_tokens) / NULLIF(SUM(duration_ms), 0), 1) AS e2e_tok_per_sec,
       ROUND(1000.0 * SUM(output_tokens) / NULLIF(SUM(duration_ms - COALESCE(ttft_ms, 0)), 0), 1) AS post_ttft_tok_per_sec
FROM requests
GROUP BY upstream_model, day
ORDER BY upstream_model, day
LIMIT 20;

-- 2. 最近请求明细
SELECT '=== 最近 20 条请求 ===' AS '';
SELECT time, upstream_model,
       duration_ms, ttft_ms,
       stream, upstream_status,
       printf('%,d', input_tokens) AS input_len,
       printf('%,d', output_tokens) AS output_len,
       printf('%,d', cached_tokens) AS cache_hit_len,
       CASE WHEN cached_tokens > 0 THEN 'hit' ELSE 'miss' END AS cache_hit,
       ROUND(100.0 * cached_tokens / NULLIF(input_tokens, 0), 1) AS cache_pct,
       printf('%,d', total_tokens) AS total_tokens,
       ROUND(1000.0 * output_tokens / NULLIF(duration_ms, 0), 1) AS e2e_tok_per_sec,
       ROUND(1000.0 * output_tokens / NULLIF(duration_ms - COALESCE(ttft_ms, 0), 0), 1) AS post_ttft_tok_per_sec,
       client_ua
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
