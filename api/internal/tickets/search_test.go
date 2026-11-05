package tickets

import (
	"slices"
	"strings"
	"testing"
	"unicode/utf16"
)

func TestSearchTerms(t *testing.T) {
	got := SearchTerms(`Pineapple  "late shipment" -excluded -"not this" relay,`)
	want := []string{"late shipment", "pineapple", "relay"}
	if !slices.Equal(got, want) {
		t.Fatalf("terms %q, want %q", got, want)
	}
	if got := SearchTerms(`-only "-"`); len(got) != 0 {
		t.Fatalf("negations only: %q", got)
	}
}

func span(text string, r Range) string {
	u := utf16.Encode([]rune(text))
	return string(utf16.Decode(u[r.Start:r.End]))
}

func TestMakeSnippetHighlightsInUTF16(t *testing.T) {
	text, hl := MakeSnippet("🍍  Pineapple\n\nshipment: PINEAPPLE <b>late</b>", []string{"pineapple"}, 200)
	if text != "🍍 Pineapple shipment: PINEAPPLE <b>late</b>" {
		t.Fatalf("text %q", text)
	}
	if len(hl) != 2 || hl[0].Start != 3 {
		t.Fatalf("ranges %+v", hl)
	}
	for _, r := range hl {
		if strings.ToLower(span(text, r)) != "pineapple" {
			t.Fatalf("range %+v covers %q", r, span(text, r))
		}
	}
}

func TestMakeSnippetWindowsAroundFirstMatch(t *testing.T) {
	long := strings.Repeat("filler words here ", 40) + "the needle is here " + strings.Repeat("tail text ", 40)
	text, hl := MakeSnippet(long, []string{"needle"}, 80)
	if !strings.HasPrefix(text, "…") || !strings.HasSuffix(text, "…") {
		t.Fatalf("expected ellipses: %q", text)
	}
	if n := len([]rune(text)); n > 82 {
		t.Fatalf("snippet too long: %d", n)
	}
	if len(hl) != 1 || span(text, hl[0]) != "needle" {
		t.Fatalf("highlight %+v in %q", hl, text)
	}
	// No match: the beginning of the text, no ranges.
	text, hl = MakeSnippet(long, []string{"absent"}, 80)
	if strings.HasPrefix(text, "…") || len(hl) != 0 {
		t.Fatalf("no-match snippet %q %+v", text, hl)
	}
}
