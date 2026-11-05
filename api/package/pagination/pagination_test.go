package pagination

import (
	"testing"
)

func TestCursorRoundTrip(t *testing.T) {
	c := Cursor{Sort: "2026-10-01T21:00:00Z", Id: "65f0c0ffee"}
	got, err := Decode(c.Encode())
	if err != nil || got == nil || *got != c {
		t.Fatalf("round trip: %+v %v", got, err)
	}
	if got, err := Decode(""); got != nil || err != nil {
		t.Fatalf("empty token: %+v %v", got, err)
	}
	for _, bad := range []string{"!!!", "eyJ2Ijo5fQ", "e30"} {
		if _, err := Decode(bad); err == nil {
			t.Errorf("token %q accepted", bad)
		}
	}
}

func TestParseLimit(t *testing.T) {
	if n, _ := ParseLimit(""); n != DefaultLimit {
		t.Fatalf("default = %d", n)
	}
	if n, err := ParseLimit("100"); err != nil || n != 100 {
		t.Fatalf("100: %d %v", n, err)
	}
	for _, s := range []string{"0", "101", "x", "-1"} {
		if _, err := ParseLimit(s); err == nil {
			t.Errorf("%q accepted", s)
		}
	}
}

func TestNewPage(t *testing.T) {
	cur := func(i int) Cursor { return Cursor{Id: string(rune('a' + i))} }
	p := NewPage([]int{0, 1, 2}, 2, cur)
	if len(p.Items) != 2 || p.NextCursor == nil {
		t.Fatalf("page = %+v", p)
	}
	next, _ := Decode(*p.NextCursor)
	if next.Id != "b" {
		t.Fatalf("next cursor = %+v", next)
	}
	last := NewPage([]int{0, 1}, 2, cur)
	if last.NextCursor != nil {
		t.Fatal("unexpected next cursor on final page")
	}
	empty := NewPage[int](nil, 2, cur)
	if empty.Items == nil {
		t.Fatal("items must serialize as [] not null")
	}
}
