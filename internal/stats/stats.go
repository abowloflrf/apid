// Package stats 在 store 之上提供请求指标的异步落盘：
// 业务方把 Record 丢进 Recorder，Recorder 内部走有界 channel + 单 worker，
// 批量 INSERT 到 requests 表。热路径只做一次 channel send，永不阻塞。
// Close 排空 channel 后退出 worker，底层 *sql.DB 由 store 关闭。
package stats

import (
	"database/sql"
	"log"
	"sync"
	"time"

	"github.com/abowloflrf/apid/internal/store"
)

// 默认 channel 容量与批大小。可在 NewRecorder 时覆盖 buffer。
const (
	defaultBuffer = 1024
	batchSize     = 64
	flushInterval = 500 * time.Millisecond
)

// Record 是单条请求的指标快照。
// 业务方按需填充字段后调用 Recorder.Record；失败请求 Usage 字段保持 nil，
// Error 字段填原因。
type Record struct {
	Time           time.Time
	Duration       time.Duration
	ClientProtocol string
	ClientPath     string
	ClientModel    string
	ClientUA       string
	ClientStatus   int
	Stream         bool

	// TTFT 是首 token 耗时，仅流式请求且收到至少一个内容增量时为正；
	// 非流式 / 失败 / 无内容时为 0，落盘写 NULL（表示"未测量"）。
	TTFT time.Duration

	UpstreamProtocol string
	UpstreamURL      string
	UpstreamModel    string
	UpstreamStatus   int

	Usage *Usage
	Error string
}

// Usage 是从上游 usage 字段抽取的 token 指标。
type Usage struct {
	InputTokens  int
	OutputTokens int
	TotalTokens  int
	CachedTokens int
}

// Recorder 异步收集请求指标并写入 store。零值不可用；用 NewRecorder 创建。
// 所有方法对 nil 接收者安全，业务方可放心 nil-check 后跳过。
type Recorder struct {
	db        *sql.DB
	ch        chan Record
	closeOnce sync.Once
	done      chan struct{}
}

// NewRecorder 创建一个绑定到 st 的 Recorder。
// st 为 nil 或未启用（Store.Enabled() == false）时返回 nil。
// buffer 是 channel 容量；<=0 时取 defaultBuffer。
func NewRecorder(st *store.Store, buffer int) *Recorder {
	if !st.Enabled() {
		return nil
	}
	if buffer <= 0 {
		buffer = defaultBuffer
	}
	r := &Recorder{
		db:   st.DB(),
		ch:   make(chan Record, buffer),
		done: make(chan struct{}),
	}
	go r.run()
	return r
}

// Record 非阻塞地提交一条记录。channel 满时丢弃并打印 warning，
// 保证请求热路径在 IO 抖动时也不阻塞。
func (r *Recorder) Record(rec Record) {
	if r == nil {
		return
	}
	select {
	case r.ch <- rec:
	default:
		log.Printf("stats: channel full, dropping record (path=%s model=%s)",
			rec.ClientPath, rec.ClientModel)
	}
}

// Close 排空 channel、停止 worker。重复调用与 nil 接收者都安全。
// 不关闭底层 *sql.DB——DB 生命周期由 store 拥有。
func (r *Recorder) Close() {
	if r == nil {
		return
	}
	r.closeOnce.Do(func() {
		close(r.ch)
		<-r.done
	})
}

// run 是单 worker goroutine：攒 batch 后批量 INSERT，定期 flush 兜底。
func (r *Recorder) run() {
	defer close(r.done)
	buf := make([]Record, 0, batchSize)
	ticker := time.NewTicker(flushInterval)
	defer ticker.Stop()

	flush := func() {
		if len(buf) == 0 {
			return
		}
		if err := writeBatch(r.db, buf); err != nil {
			log.Printf("stats: batch insert failed: %v", err)
		}
		buf = buf[:0]
	}

	for {
		select {
		case rec, ok := <-r.ch:
			if !ok {
				flush()
				return
			}
			buf = append(buf, rec)
			if len(buf) >= batchSize {
				flush()
			}
		case <-ticker.C:
			flush()
		}
	}
}

const insertSQL = `INSERT INTO requests (
	time, duration_ms, ttft_ms,
	client_protocol, client_path, client_model, client_ua,
	upstream_protocol, upstream_url, upstream_model,
	stream, client_status, upstream_status,
	input_tokens, output_tokens, total_tokens, cached_tokens,
	error
) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`

// writeBatch 在一个事务里批量 INSERT 所有记录。
func writeBatch(db *sql.DB, records []Record) error {
	if len(records) == 0 {
		return nil
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	stmt, err := tx.Prepare(insertSQL)
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	defer stmt.Close()

	for _, rec := range records {
		var (
			inputTok, outputTok, totalTok, cachedTok int
		)
		if rec.Usage != nil {
			inputTok = rec.Usage.InputTokens
			outputTok = rec.Usage.OutputTokens
			totalTok = rec.Usage.TotalTokens
			cachedTok = rec.Usage.CachedTokens
		}
		stream := 0
		if rec.Stream {
			stream = 1
		}
		if _, err := stmt.Exec(
			rec.Time.UTC().Format(time.RFC3339Nano),
			rec.Duration.Milliseconds(),
			ttftMillis(rec.TTFT),
			rec.ClientProtocol,
			rec.ClientPath,
			rec.ClientModel,
			rec.ClientUA,
			rec.UpstreamProtocol,
			rec.UpstreamURL,
			rec.UpstreamModel,
			stream,
			rec.ClientStatus,
			rec.UpstreamStatus,
			inputTok, outputTok, totalTok, cachedTok,
			nullString(rec.Error),
		); err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// ttftMillis 把 TTFT 转成毫秒；未测量(<=0)时返回 nil 写 NULL，
// 与 duration_ms(总是有值) 区分开，避免把"没测到"误读成"0ms"。
func ttftMillis(d time.Duration) any {
	if d <= 0 {
		return nil
	}
	return d.Milliseconds()
}
