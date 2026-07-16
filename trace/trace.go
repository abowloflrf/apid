// Package trace 实现 TRACE 级别的请求落盘：
// 开启后，每条请求把 HTTP method、URI 与请求体 JSON 各存成一个文件，
// 方便离线 DEBUG。同一条请求可落盘多种形态（kind）：原始的 Responses 请求
// 与转换后的 Chat Completions 请求，二者共享时间戳+序号前缀以便配对。
// 默认关闭，零开销。
package trace

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"
)

// Tracer 把请求落盘到 dir。dir 为空表示禁用。
type Tracer struct {
	dir string
	log *slog.Logger
	seq atomic.Uint64
}

// New 创建 Tracer。dir 为空则返回一个禁用的 Tracer；
// 非空时确保目录存在，创建失败则降级为禁用并打日志。
// logger 为 nil 时回退到 slog.Default()。
func New(dir string, logger *slog.Logger) *Tracer {
	if logger == nil {
		logger = slog.Default()
	}
	if dir == "" {
		return &Tracer{log: logger}
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		logger.Warn("trace dir create failed, tracing disabled", "dir", dir, "err", err)
		return &Tracer{log: logger}
	}
	logger.Info("trace enabled", "dir", dir)
	return &Tracer{dir: dir, log: logger}
}

// Enabled 表示当前 Tracer 是否会落盘。
func (t *Tracer) Enabled() bool { return t != nil && t.dir != "" }

// traceRecord 是单个 trace 文件的内容结构。
type traceRecord struct {
	Time   string `json:"time"`
	Method string `json:"method"`
	URI    string `json:"uri"`
	Kind   string `json:"kind"` // 请求形态：responses(原始) / chat(转换后)
	Body   any    `json:"body"` // 合法 JSON 时为原样对象，否则为原始字符串
}

// Entry 表示一次请求的 trace 上下文。同一 Entry 落盘的多份文件共享
// 时间戳与序号前缀，文件名仅以 kind 后缀区分，便于把原始请求与转换后的
// 请求配对查看。禁用时为 nil，其方法均可安全调用。
type Entry struct {
	t      *Tracer
	prefix string // 文件名前缀：{时间戳}-{序号}-{method}
	time   string // RFC3339Nano 时间，写入记录
	method string
	uri    string
}

// Begin 开始一次请求的 trace 上下文；禁用时返回 nil。
// 同一请求的不同形态调用 Entry.Dump 落盘，共享此处分配的时间戳与序号。
func (t *Tracer) Begin(method, uri string) *Entry {
	if !t.Enabled() {
		return nil
	}
	now := time.Now()
	seq := t.seq.Add(1)
	return &Entry{
		t:      t,
		prefix: fmt.Sprintf("%s-%06d-%s", now.Format("20060102-150405.000000"), seq, method),
		time:   now.Format(time.RFC3339Nano),
		method: method,
		uri:    uri,
	}
}

// Dump 把请求体某个形态(kind)写入一个独立文件，返回文件路径（禁用时返回空串）。
// 文件名形如 20060102-150405.000000-000001-POST-responses.json。
func (e *Entry) Dump(kind string, body []byte) string {
	if e == nil {
		return ""
	}
	name := fmt.Sprintf("%s-%s.json", e.prefix, kind)
	path := filepath.Join(e.t.dir, name)

	rec := traceRecord{
		Time:   e.time,
		Method: e.method,
		URI:    e.uri,
		Kind:   kind,
	}
	if json.Valid(body) {
		rec.Body = json.RawMessage(body)
	} else {
		rec.Body = string(body)
	}

	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		e.t.log.Warn("trace marshal record failed", "err", err)
		return ""
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		e.t.log.Warn("trace write failed", "path", path, "err", err)
		return ""
	}
	return path
}
