package http

import "testing"

func TestStreamCounterBoundsPerKey(t *testing.T) {
	s := newStreamCounter(2)
	first, second := s.acquire("a"), s.acquire("a")
	if !first || !second {
		t.Fatal("first two streams refused")
	}
	if s.acquire("a") {
		t.Fatal("third stream accepted")
	}
	if !s.acquire("b") {
		t.Fatal("other key refused")
	}
	s.release("a")
	if !s.acquire("a") {
		t.Fatal("released slot not reusable")
	}
	s.release("a")
	s.release("a")
	s.release("b")
	if len(s.open) != 0 {
		t.Fatalf("leaked keys: %v", s.open)
	}
}
