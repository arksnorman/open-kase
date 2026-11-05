package http

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/arksnorman/open-kase/api/internal/tickets"
)

func registerSearch(p *gin.RouterGroup, a *api) {
	p.GET("/search", a.searchTickets)
}

// ---- Search ----

type highlightDTO struct {
	Start int `json:"start"`
	End   int `json:"end"`
}

// snippetDTO is plain text with highlight ranges (UTF-16 code units). It
// never contains server-added markup.
type snippetDTO struct {
	Field      string         `json:"field" enum:"subject,message"`
	MessageId  *string        `json:"messageId"`
	Visibility *string        `json:"visibility" enum:"public,agent"`
	Text       string         `json:"text"`
	Highlights []highlightDTO `json:"highlights"`
}

type searchHitDTO struct {
	Ticket   ticketDTO    `json:"ticket"`
	Score    float64      `json:"score"`
	Snippets []snippetDTO `json:"snippets"`
}

type searchPageDTO struct {
	Items      []searchHitDTO `json:"items"`
	NextCursor *string        `json:"nextCursor"`
	// Total counts matching tickets the caller may see.
	Total       int64 `json:"total"`
	TotalCapped bool  `json:"totalCapped"`
}

func toSearchPage(r tickets.SearchResult) searchPageDTO {
	out := searchPageDTO{Items: make([]searchHitDTO, 0, len(r.Page.Items)), NextCursor: r.Page.NextCursor, Total: r.Total, TotalCapped: r.TotalCapped}
	for _, h := range r.Page.Items {
		hit := searchHitDTO{Ticket: toTicket(h.Ticket), Score: h.Score, Snippets: make([]snippetDTO, 0, len(h.Snippets))}
		for _, s := range h.Snippets {
			sn := snippetDTO{Field: s.Field, MessageId: nullable(s.MessageId), Visibility: nullable(s.Visibility), Text: s.Text,
				Highlights: make([]highlightDTO, 0, len(s.Highlights))}
			for _, r := range s.Highlights {
				sn.Highlights = append(sn.Highlights, highlightDTO{Start: r.Start, End: r.End})
			}
			hit.Snippets = append(hit.Snippets, sn)
		}
		out.Items = append(out.Items, hit)
	}
	return out
}

func (a *api) searchTickets(c *gin.Context) {
	s, ok := a.scope(c)
	if !ok {
		return
	}
	after, limit, ok := pageParams(c)
	if !ok {
		return
	}
	r, err := a.Tickets.Search(c.Request.Context(), s, tickets.SearchInput{Q: c.Query("q"), After: after, Limit: limit})
	respond(c, http.StatusOK, toSearchPage(r), err)
}
