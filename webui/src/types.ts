// Shared shapes for the /stats/* JSON API. Field names mirror the Go side;
// numeric fields may be absent/zero for rows without data.

export interface Summary {
  requests: number;
  stream_requests: number;
  errors: number;
  error_pct: number;
  total_tokens: number;
  input_tokens: number;
  output_tokens: number;
  cached_tokens: number;
  cache_pct: number;
  avg_duration_ms: number;
  avg_ttft_ms: number;
  max_duration_ms: number;
  e2e_tok_per_sec: number;
  post_ttft_tok_per_sec: number;
}

export interface TimeBucket {
  bucket: string;
  requests: number;
  errors: number;
  stream_requests: number;
  input_tokens: number;
  cached_tokens: number;
  output_tokens: number;
  total_tokens: number;
  cache_pct: number;
  error_pct: number;
  avg_duration_ms: number;
  max_duration_ms: number;
  avg_ttft_ms: number;
  max_ttft_ms: number;
  e2e_tok_per_sec: number;
  post_ttft_tok_per_sec: number;
}

export interface ByModelRow {
  upstream_model: string;
  requests: number;
  errors: number;
  error_pct: number;
  input_tokens: number;
  output_tokens: number;
  cached_tokens: number;
  total_tokens: number;
  cache_pct: number;
  avg_duration_ms: number;
  avg_ttft_ms: number;
  e2e_tok_per_sec: number;
}

export interface RequestRow {
  time: string;
  client_protocol: string;
  client_model?: string;
  upstream_model: string;
  client_ua?: string;
  stream: boolean;
  client_status: number;
  upstream_status?: number;
  duration_ms: number;
  ttft_ms?: number;
  input_tokens: number;
  output_tokens: number;
  cached_tokens: number;
  total_tokens: number;
  upstream_url?: string;
  error?: string;
}

export interface Options {
  models: string[];
  protocols: string[];
  min_time?: string;
  max_time?: string;
}

// ---- topology ----

export interface Topology {
  listen: string;
  client_auth: boolean;
  trace: boolean;
  storage: boolean;
  search?: { provider: string; path: string; base_url?: string } | null;
  upstreams: UpstreamInfo[];
  routes: RouteInfo[];
}

export interface UpstreamInfo {
  name: string;
  protocol: string;
  base_url: string;
  path: string;
  endpoint: string;
  supports_responses: boolean;
  responses_endpoint?: string;
  model: string;
  auth: string;
  auth_header: string;
  ref_count: number;
}

export interface RouteInfo {
  path: string;
  input_protocol: string;
  rules: RuleInfo[];
}

export interface RuleInfo {
  match: string;
  match_kind: string;
  upstream: string;
  upstream_protocol: string;
  mode: string;
  via_responses: boolean;
  model_source: string;
  effective_model: string;
  endpoint: string;
  broken: boolean;
}

// ---- live ----

export interface LiveRequest {
  id: number;
  client_model?: string;
  upstream_model?: string;
  client_protocol: string;
  upstream_protocol: string;
  mode: string;
  stream: boolean;
  ttft_ms?: number | null;
  start: number;
  path?: string;
  upstream?: string;
  client_ua?: string;
  input_tokens: number;
  output_tokens: number;
  input_est?: boolean;
  output_est?: boolean;
}

export interface LiveSnapshot {
  now: number;
  requests: LiveRequest[];
}

// ---- agent sessions ----

export interface AgentSession {
  id: string;
  title: string;
  created_at_ms: number;
  updated_at_ms: number;
  cwd: string;
  model: string;
  reasoning_effort: string;
  tokens_used: number;
  archived: boolean;
  cli_version: string;
  rollout_path: string;
  tool: string; // codex | claude | pi | opencode
  input_tokens: number;
  output_tokens: number;
  cache_read_tokens: number;
  cache_write_tokens: number;
  cache_hit_rate: number | null; // 0..1
}

export interface SessionsSummary {
  sessions: number;
  total_tokens: number;
  input_tokens: number;
  output_tokens: number;
  cache_read_tokens: number;
  cache_write_tokens: number;
  cache_pct: number | null; // 0..1
}

export interface SessionsSource {
  tool: string;
  label: string;
  desc: string;
}

export interface SessionsResponse {
  sources: SessionsSource[];
  total: number;
  limit: number;
  offset: number;
  with_tokens: boolean;
  sessions: AgentSession[];
  summary: SessionsSummary;
  generated_ms: number;
}
