// Package store 提供项目通用 SQLite 存储：
// Open 打开一个 SQLite 数据库文件并跑 schema 迁移；Close 优雅关闭。
// 业务包（stats 等）通过 Store.DB() 拿到 *sql.DB 来执行各自的 SQL。
//
// 设计：单一 owner。Store 拥有 *sql.DB 的生命周期，业务包只借用、不关闭。
// schema 集中维护，所有表的新建 / 索引都加在 schema 字符串里，Open 时一次性
// 应用；多进程 / 多次启动时 IF NOT EXISTS 保证幂等。
package store

import (
	"database/sql"
	"fmt"

	_ "modernc.org/sqlite"
)

const driverName = "sqlite"

// Store 持有一个已打开、已迁移的 SQLite 数据库。
// 零值不可用；用 Open 创建。
type Store struct {
	db   *sql.DB
	path string
}

// schema 集中维护所有表的建表语句。新表都在这里加，业务包不直接管 DDL。
const schema = `
CREATE TABLE IF NOT EXISTS requests (
    id                INTEGER PRIMARY KEY AUTOINCREMENT,
    time              TEXT    NOT NULL,
    duration_ms       INTEGER NOT NULL,
    ttft_ms           INTEGER,
    client_protocol   TEXT    NOT NULL,
    client_path       TEXT    NOT NULL,
    client_model      TEXT    NOT NULL,
    client_ua         TEXT    NOT NULL DEFAULT '',
    session_id        TEXT,
    upstream_protocol TEXT    NOT NULL,
    upstream_url      TEXT    NOT NULL,
    upstream_model    TEXT    NOT NULL,
    stream            INTEGER NOT NULL,
    client_status     INTEGER NOT NULL,
    upstream_status   INTEGER NOT NULL DEFAULT 0,
    input_tokens      INTEGER NOT NULL DEFAULT 0,
    output_tokens     INTEGER NOT NULL DEFAULT 0,
    total_tokens      INTEGER NOT NULL DEFAULT 0,
    cached_tokens     INTEGER NOT NULL DEFAULT 0,
    error             TEXT
);
CREATE INDEX IF NOT EXISTS idx_requests_time           ON requests(time);
CREATE INDEX IF NOT EXISTS idx_requests_client_model   ON requests(client_model);
CREATE INDEX IF NOT EXISTS idx_requests_upstream_model ON requests(upstream_model);
`

// Open 打开 path 处的 SQLite 文件，应用 PRAGMA 并跑 schema 迁移。
// path == "" 时返回 (nil, nil)，表示不启用存储、调用方应跳过所有持久化。
func Open(path string) (*Store, error) {
	if path == "" {
		return nil, nil
	}
	db, err := sql.Open(driverName, path)
	if err != nil {
		return nil, fmt.Errorf("store: open %q failed: %w", path, err)
	}
	// SQLite 写并发有限；限制连接数避免 "database is locked" 频繁重试。
	// WAL 模式下多读单写，单连接即可。
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	pragmas := []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA synchronous=NORMAL",
		"PRAGMA foreign_keys=ON",
		"PRAGMA busy_timeout=5000",
	}
	for _, p := range pragmas {
		if _, err := db.Exec(p); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("store: %s failed: %w", p, err)
		}
	}

	if _, err := db.Exec(schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: apply schema failed: %w", err)
	}
	if err := ensureRequestColumns(db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: migrate requests failed: %w", err)
	}

	return &Store{db: db, path: path}, nil
}

// ensureRequestColumns adds columns introduced after the initial schema. The
// CREATE TABLE above only handles new databases; existing tables need explicit
// ALTER TABLE migrations.
func ensureRequestColumns(db *sql.DB) error {
	const (
		column = "session_id"
		alter  = "ALTER TABLE requests ADD COLUMN session_id TEXT"
	)
	exists, err := requestColumnExists(db, column)
	if err != nil {
		return err
	}
	if exists {
		return nil
	}
	if _, err := db.Exec(alter); err != nil {
		// Another process may have added the column after our check.
		exists, checkErr := requestColumnExists(db, column)
		if checkErr == nil && exists {
			return nil
		}
		return fmt.Errorf("add %s: %w", column, err)
	}
	return nil
}

func requestColumnExists(db *sql.DB, want string) (bool, error) {
	rows, err := db.Query("PRAGMA table_info(requests)")
	if err != nil {
		return false, fmt.Errorf("inspect columns: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			cid         int
			name, typ   string
			notNull, pk int
			defaultVal  sql.NullString
		)
		if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultVal, &pk); err != nil {
			return false, fmt.Errorf("scan columns: %w", err)
		}
		if name == want {
			return true, nil
		}
	}
	if err := rows.Err(); err != nil {
		return false, fmt.Errorf("inspect columns: %w", err)
	}
	return false, nil
}

// Enabled 表示是否真的打开了数据库。
func (s *Store) Enabled() bool { return s != nil && s.db != nil }

// DB 返回原生 *sql.DB 供业务包使用。仅在 Enabled() 为 true 时可调用。
func (s *Store) DB() *sql.DB { return s.db }

// Path 返回数据库文件路径。
func (s *Store) Path() string { return s.path }

// Close 关闭底层连接。
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}
