package store

import (
	"path/filepath"
	"testing"
)

func TestOpenClose(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	if !s.Enabled() {
		t.Fatal("expected Enabled() to be true")
	}
	if s.DB() == nil {
		t.Fatal("expected non-nil DB")
	}
	if s.Path() != path {
		t.Errorf("Path = %q, want %q", s.Path(), path)
	}
	if err := s.Close(); err != nil {
		t.Errorf("Close failed: %v", err)
	}
}

func TestOpenEmpty(t *testing.T) {
	s, err := Open("")
	if err != nil {
		t.Fatalf("Open(\"\") should not error: %v", err)
	}
	if s != nil {
		t.Errorf("expected nil store for empty path, got %+v", s)
	}
}

func TestOpenInMemory(t *testing.T) {
	// 测试代码常用 :memory:，应能正常打开、应用 schema、可查询。
	s, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open(:memory:) failed: %v", err)
	}
	defer s.Close()
	var n int
	if err := s.DB().QueryRow("SELECT count(*) FROM sqlite_master WHERE type='table' AND name='requests'").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("requests 表数量 = %d, 期望 1", n)
	}
}

func TestSchemaIdempotent(t *testing.T) {
	// 多次 Open 同路径，schema 迁移应幂等（IF NOT EXISTS）。
	path := filepath.Join(t.TempDir(), "test.db")
	s1, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	_ = s1.Close()
	s2, err := Open(path)
	if err != nil {
		t.Fatalf("second Open failed: %v", err)
	}
	defer s2.Close()
	var x int
	if err := s2.DB().QueryRow("SELECT 1").Scan(&x); err != nil {
		t.Errorf("query after reopen failed: %v", err)
	}
}

func TestCloseNil(t *testing.T) {
	// nil 接收者 Close 不应 panic。
	var s *Store
	if err := s.Close(); err != nil {
		t.Errorf("nil Close should not error: %v", err)
	}
}
