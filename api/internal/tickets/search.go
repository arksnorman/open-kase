package tickets

import (
	"context"
	"math"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/arksnorman/open-kase/api/internal/access"
	"github.com/arksnorman/open-kase/api/package/fault"
	"github.com/arksnorman/open-kase/api/package/pagination"
	"github.com/arksnorman/open-kase/api/package/validation"
)

// MaxSearchCandidates bounds how many matching tickets (and, per audience,
// how many matching messages) one search considers. Totals beyond it are
// reported as capped rather than counted.
const MaxSearchCandidates = 1000

// SnippetWidth is the snippet length in Unicode code points (before the
// ellipses).
const SnippetWidth = 200

// maxHighlights bounds the highlight ranges per snippet.
const maxHighlights = 20

// SearchInput is a full-text search request.
type SearchInput struct {
	Q     string
	After *pagination.Cursor
	Limit int
}

// Range is a highlighted span of a snippet, in UTF-16 code units (the
// string indices JavaScript uses), end exclusive.
type Range struct {
	Start int
	End   int
}

// Snippet is plain text taken from content the caller may read, with the
// matched terms marked by ranges. It never contains markup added by the
// server.
type Snippet struct {
	Field      string // "subject" or "message"
	MessageId  string
	Visibility string
	Text       string
	Highlights []Range
}

// SearchHit is one matching ticket.
type SearchHit struct {
	Ticket   Ticket
	Score    float64
	Snippets []Snippet
}

// SearchResult is one page of hits plus the authorized total.
type SearchResult struct {
	Page pagination.Page[SearchHit]
	// Total counts matching tickets the caller may see, computed after
	// authorization and audience filtering.
	Total int64
	// TotalCapped means the candidate bound was reached; Total is then a
	// lower bound.
	TotalCapped bool
}

// SearchQuery is the authorized repository query. Base carries the project,
// and the access filter; Audiences lists the
// message audiences whose bodies may match (empty: subjects only).
type SearchQuery struct {
	Base          Query
	Text          string
	Audiences     []string
	MaxCandidates int
}

// SearchCandidate is a matching ticket the caller may see, with its best
// matching message among the allowed audiences.
type SearchCandidate struct {
	TicketId       string
	Score          float64
	SubjectMatched bool
	MessageId      string
	Visibility     string
}

// searchAudiences returns the message audiences the scope may search. A
// principal that cannot read messages searches subjects only.
func searchAudiences(scope *access.ProjectScope) []string {
	if !scope.Can(access.CapMessageReadPublic) {
		return nil
	}
	aud, err := readableAudiences(scope)
	if err != nil {
		return nil
	}
	return aud
}

// Search finds tickets whose subject or readable messages match q. The
// access filter and the audience restriction are part of the database
// queries, so agent notes never match, count or produce snippets for a
// principal that cannot read them.
func (s *Service) Search(ctx context.Context, scope *access.ProjectScope, in SearchInput) (SearchResult, error) {
	acc, err := readAccess(scope)
	if err != nil {
		return SearchResult{}, err
	}
	q := strings.TrimSpace(in.Q)
	var v validation.Collector
	if q == "" {
		v.Add("q", validation.CodeRequired, "Enter one or more words to search for.")
	} else {
		v.String(validation.SearchQuery, "q", q)
	}
	if err := v.Err(); err != nil {
		return SearchResult{}, err
	}
	terms := SearchTerms(q)
	if len(terms) == 0 {
		return SearchResult{}, fault.Field("q", validation.CodeInvalidFormat, "Enter at least one word that is not excluded with a minus sign.")
	}
	audiences := searchAudiences(scope)
	cands, capped, err := s.repo.Search(ctx, SearchQuery{
		Base:      Query{ProjectId: scope.Project.Id, ViewerId: viewerID(scope), Access: acc},
		Text:      q,
		Audiences: audiences, MaxCandidates: MaxSearchCandidates,
	})
	if err != nil {
		return SearchResult{}, err
	}
	// A query that is a ticket reference ("1042" or "#1042")
	// puts that ticket first when the caller may read it.
	if _, n, ok := ParseRef(q); ok && n > 0 {
		if t, err := s.load(ctx, scope, q); err == nil {
			cands = slices.DeleteFunc(cands, func(c SearchCandidate) bool { return c.TicketId == t.Id })
			cands = slices.Insert(cands, 0, SearchCandidate{TicketId: t.Id, Score: math.MaxFloat64})
		} else if fault.As(err).Kind != fault.KindNotFound {
			return SearchResult{}, err
		}
	}
	out := SearchResult{Total: int64(len(cands)), TotalCapped: capped}
	start := 0
	if in.After != nil {
		score, perr := strconv.ParseFloat(in.After.Sort, 64)
		if perr != nil || !validation.ValidID(in.After.Id) {
			return SearchResult{}, fault.Field("cursor", "invalidCursor", "The page cursor is invalid or expired.")
		}
		start = len(cands)
		for i, c := range cands {
			if c.Score < score || (c.Score == score && c.TicketId < in.After.Id) {
				start = i
				break
			}
		}
	}
	end := min(len(cands), start+in.Limit+1)
	window := cands[start:end]
	ids := make([]string, 0, len(window))
	var msgIDs []string
	for _, c := range window {
		ids = append(ids, c.TicketId)
		if c.MessageId != "" && slices.Contains(audiences, c.Visibility) {
			msgIDs = append(msgIDs, c.MessageId)
		}
	}
	rows, err := s.repo.ListByIDs(ctx, scope.Project.Id, ids, viewerID(scope))
	if err != nil {
		return SearchResult{}, err
	}
	byID := make(map[string]Ticket, len(rows))
	for _, t := range rows {
		byID[t.Id] = t
	}
	var bodies map[string]Message
	if len(msgIDs) > 0 {
		if bodies, err = s.repo.MessageBodies(ctx, scope.Project.Id, msgIDs); err != nil {
			return SearchResult{}, err
		}
	}
	hits := make([]SearchHit, 0, len(window))
	for _, c := range window {
		t, ok := byID[c.TicketId]
		// Defence in depth: the repository already applied these filters.
		if !ok || !acc.allows(t) {
			continue
		}
		hit := SearchHit{Ticket: s.present(scope, t), Score: c.Score, Snippets: []Snippet{}}
		if text, hl := MakeSnippet(t.Subject, terms, SnippetWidth); c.SubjectMatched || len(hl) > 0 {
			hit.Snippets = append(hit.Snippets, Snippet{Field: "subject", Text: text, Highlights: hl})
		}
		if m, ok := bodies[c.MessageId]; ok && m.TicketId == t.Id && slices.Contains(audiences, m.Visibility) {
			text, hl := MakeSnippet(m.Body, terms, SnippetWidth)
			hit.Snippets = append(hit.Snippets, Snippet{Field: "message", MessageId: m.Id, Visibility: m.Visibility, Text: text, Highlights: hl})
		}
		hits = append(hits, hit)
	}
	out.Page = pagination.NewPage(hits, in.Limit, func(h SearchHit) pagination.Cursor {
		return pagination.Cursor{Sort: strconv.FormatFloat(h.Score, 'g', -1, 64), Id: h.Ticket.Id}
	})
	return out, nil
}

// SearchTerms extracts the words and quoted phrases of a search query for
// highlighting, lower-cased. Negated words (-word) are dropped.
func SearchTerms(q string) []string {
	var out []string
	add := func(t string) {
		t = strings.TrimFunc(strings.ToLower(t), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
		if t != "" && !slices.Contains(out, t) {
			out = append(out, t)
		}
	}
	for {
		i := strings.IndexByte(q, '"')
		if i < 0 {
			break
		}
		j := strings.IndexByte(q[i+1:], '"')
		if j < 0 {
			q = q[:i] + " " + q[i+1:]
			break
		}
		phrase := q[i+1 : i+1+j]
		if !(i > 0 && q[i-1] == '-') {
			add(phrase)
		}
		q = q[:i] + " " + q[i+1+j+1:]
	}
	for _, w := range strings.Fields(q) {
		if strings.HasPrefix(w, "-") {
			continue
		}
		add(w)
	}
	// Longest first, so overlapping terms highlight the longer match.
	slices.SortStableFunc(out, func(a, b string) int { return utf8.RuneCountInString(b) - utf8.RuneCountInString(a) })
	return out
}

// MakeSnippet returns up to width code points of text around the first
// match of any term (whitespace collapsed, with ellipses where cut) and the
// highlight ranges in UTF-16 code units. Matching is case-insensitive.
func MakeSnippet(text string, terms []string, width int) (string, []Range) {
	src := []rune(strings.Join(strings.Fields(text), " "))
	low := make([]rune, len(src))
	for i, r := range src {
		low[i] = unicode.ToLower(r)
	}
	type span struct{ s, e int }
	var matches []span
	tr := make([][]rune, 0, len(terms))
	for _, t := range terms {
		if rs := []rune(t); len(rs) > 0 {
			tr = append(tr, rs)
		}
	}
	for i := 0; i < len(low); i++ {
		for _, t := range tr {
			if i+len(t) <= len(low) && slices.Equal(low[i:i+len(t)], t) {
				matches = append(matches, span{i, i + len(t)})
				i += len(t) - 1
				break
			}
		}
	}
	start, end := 0, len(src)
	if len(src) > width {
		first := 0
		if len(matches) > 0 {
			first = matches[0].s
		}
		start = max(0, first-width/4)
		end = min(len(src), start+width)
		if end-start < width {
			start = max(0, end-width)
		}
		// Prefer cutting at word boundaries near the edges.
		if start > 0 {
			for k := start; k < min(start+15, end); k++ {
				if src[k] == ' ' && (len(matches) == 0 || k < matches[0].s) {
					start = k + 1
					break
				}
			}
		}
		if end < len(src) {
			for k := end - 1; k > max(end-15, start); k-- {
				if src[k] == ' ' {
					end = k
					break
				}
			}
		}
	}
	var b strings.Builder
	prefix := 0
	if start > 0 {
		b.WriteString("…")
		prefix = 1 // U+2026 is one UTF-16 code unit
	}
	b.WriteString(string(src[start:end]))
	if end < len(src) {
		b.WriteString("…")
	}
	ranges := []Range{}
	for _, m := range matches {
		if m.s < start || m.e > end {
			continue
		}
		s := prefix + utf16Len(src[start:m.s])
		ranges = append(ranges, Range{Start: s, End: s + utf16Len(src[m.s:m.e])})
		if len(ranges) == maxHighlights {
			break
		}
	}
	return b.String(), ranges
}

func utf16Len(rs []rune) int {
	n := 0
	for _, r := range rs {
		n += utf16.RuneLen(r)
	}
	return n
}
