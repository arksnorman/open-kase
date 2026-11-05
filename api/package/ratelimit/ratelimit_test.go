package ratelimit

import (
	"testing"
	"time"
)

func TestLimiterBurstRefillAndBound(t *testing.T) {
	now := time.Unix(0, 0)
	l := New(60, 3, 2, func() time.Time { return now })
	for i := 0; i < 3; i++ {
		if w := l.Allow("a"); w != 0 {
			t.Fatalf("call %d limited", i)
		}
	}
	if w := l.Allow("a"); w <= 0 || w > time.Second {
		t.Fatalf("fourth call wait %v, want ~1s", w)
	}
	now = now.Add(time.Second)
	if w := l.Allow("a"); w != 0 {
		t.Fatal("token not refilled")
	}
	l.Allow("b")
	l.Allow("c") // evicts to stay within maxKeys
	if l.Len() > 2 {
		t.Fatalf("limiter holds %d keys", l.Len())
	}
}

func TestLimiterKeysAreIndependent(t *testing.T) {
	now := time.Unix(0, 0)
	l := New(1, 1, 10, func() time.Time { return now })
	if l.Allow("a") != 0 || l.Allow("b") != 0 {
		t.Fatal("first calls limited")
	}
	if w := l.Allow("a"); w < 59*time.Second {
		t.Fatalf("wait %v, want ~60s", w)
	}
}
