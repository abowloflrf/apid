package reasoning

import (
	"testing"
	"time"
)

func TestByCallIDRoundTrip(t *testing.T) {
	c := New(8, time.Hour)
	c.SaveCall("call_1", "deep thought")
	if got := c.ByCallID("call_1"); got != "deep thought" {
		t.Fatalf("ByCallID = %q, want %q", got, "deep thought")
	}
	if got := c.ByCallID("missing"); got != "" {
		t.Errorf("ByCallID(missing) = %q, want empty", got)
	}
}

func TestByContentRoundTrip(t *testing.T) {
	c := New(8, time.Hour)
	c.SaveContent("hello world", "reasoning R")
	if got := c.ByContent("hello world"); got != "reasoning R" {
		t.Fatalf("ByContent = %q, want %q", got, "reasoning R")
	}
	if got := c.ByContent("other"); got != "" {
		t.Errorf("ByContent(other) = %q, want empty", got)
	}
}

func TestEmptyInputsIgnored(t *testing.T) {
	c := New(8, time.Hour)
	c.SaveCall("", "x")
	c.SaveCall("call", "")
	c.SaveContent("", "x")
	c.SaveContent("content", "")
	if got := c.ByCallID("call"); got != "" {
		t.Errorf("empty reasoning stored: %q", got)
	}
	if got := c.ByContent("content"); got != "" {
		t.Errorf("empty reasoning stored: %q", got)
	}
}

func TestNilCacheSafe(t *testing.T) {
	var c *Cache
	c.SaveCall("a", "b")    // must not panic
	c.SaveContent("a", "b") // must not panic
	if got := c.ByCallID("a"); got != "" {
		t.Errorf("nil cache returned %q", got)
	}
	if got := c.ByContent("a"); got != "" {
		t.Errorf("nil cache returned %q", got)
	}
}

func TestCapacityEviction(t *testing.T) {
	c := New(2, time.Hour)
	c.SaveCall("a", "1")
	c.SaveCall("b", "2")
	c.SaveCall("c", "3") // evicts least-recently-used "a"
	if got := c.ByCallID("a"); got != "" {
		t.Errorf("a should be evicted, got %q", got)
	}
	if got := c.ByCallID("c"); got != "3" {
		t.Errorf("c = %q, want 3", got)
	}
}

func TestLRUTouchOnGet(t *testing.T) {
	c := New(2, time.Hour)
	c.SaveCall("a", "1")
	c.SaveCall("b", "2")
	_ = c.ByCallID("a")  // touch a, so b becomes least-recently-used
	c.SaveCall("c", "3") // evicts b
	if got := c.ByCallID("b"); got != "" {
		t.Errorf("b should be evicted, got %q", got)
	}
	if got := c.ByCallID("a"); got != "1" {
		t.Errorf("a should survive, got %q", got)
	}
}

func TestTTLExpiry(t *testing.T) {
	c := New(8, time.Nanosecond)
	c.SaveCall("a", "1")
	time.Sleep(time.Millisecond)
	if got := c.ByCallID("a"); got != "" {
		t.Errorf("a should be expired, got %q", got)
	}
}
