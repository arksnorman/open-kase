package http

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/arksnorman/open-kase/api/internal/macros"
	"github.com/arksnorman/open-kase/api/internal/reports"
	"github.com/arksnorman/open-kase/api/internal/sla"
	"github.com/arksnorman/open-kase/api/internal/tickets"
)

func registerProductivity(p *gin.RouterGroup, a *api) {
	p.GET("/tags", a.listTags)
	p.POST("/tags", a.idempotent(a.createTag))
	p.DELETE("/tags/:name", a.deleteTag)
	p.GET("/tickets/:t/sla", a.ticketSLA)

	p.GET("/macros", a.listMacros)
	p.POST("/macros", a.idempotent(a.createMacro))
	p.GET("/macros/:m", a.getMacro)
	p.PATCH("/macros/:m", a.patchMacro)
	p.DELETE("/macros/:m", a.deleteMacro)
	p.GET("/tickets/:t/macros/:m/preview", a.previewMacro)

	p.GET("/sla", a.getSLA)
	p.PUT("/sla", a.putSLA)

	p.GET("/reports/summary", a.reportSummary)
}

// ---- Ticket SLA DTO ----

type slaClockDTO struct {
	Kind           string     `json:"kind" enum:"firstResponse"`
	State          string     `json:"state" enum:"running,met,stopped"`
	BudgetSeconds  *int64     `json:"budgetSeconds"` // null: no target for the priority
	ElapsedSeconds int64      `json:"elapsedSeconds"`
	DueAt          *time.Time `json:"dueAt"`
	WarnAt         *time.Time `json:"warnAt"`
	MetAt          *time.Time `json:"metAt"`
	BreachedAt     *time.Time `json:"breachedAt"`
}

type slaDTO struct {
	Kind     string        `json:"kind" enum:"firstResponse"`
	State    string        `json:"state" enum:"ok,dueSoon,breached,met"`
	DueAt    *time.Time    `json:"dueAt"`
	Breached bool          `json:"breached"`
	Cycle    int           `json:"cycle"`
	Version  int64         `json:"version" doc:"The SLA version the cycle started with."`
	Targets  []slaClockDTO `json:"targets"`
}

func toSLA(s *sla.Summary) *slaDTO {
	if s == nil {
		return nil
	}
	out := &slaDTO{Kind: string(s.Kind), State: s.State, DueAt: s.DueAt, Breached: s.Breached,
		Cycle: s.Cycle, Version: s.Version, Targets: []slaClockDTO{}}
	for _, c := range s.Clocks {
		d := slaClockDTO{Kind: string(c.Kind), State: string(c.State), ElapsedSeconds: c.ElapsedSeconds,
			DueAt: c.DueAt, WarnAt: c.WarnAt, MetAt: c.MetAt, BreachedAt: c.BreachedAt}
		if c.BudgetSeconds > 0 {
			b := c.BudgetSeconds
			d.BudgetSeconds = &b
		}
		out.Targets = append(out.Targets, d)
	}
	return out
}

type slaCycleDTO struct {
	Cycle     int        `json:"cycle"`
	Current   bool       `json:"current"`
	Priority  int        `json:"priority"`
	Version   int64      `json:"version" doc:"The SLA version the cycle started with."`
	StartedAt time.Time  `json:"startedAt"`
	ClosedAt  *time.Time `json:"closedAt"`
	// ClosedReason is null while the cycle is open.
	ClosedReason *string `json:"closedReason" enum:"met,resolved,archived,slaOff"`
	Breaches     []struct {
		Kind string    `json:"kind"`
		At   time.Time `json:"at"`
	} `json:"breaches"`
	Targets []slaClockDTO `json:"targets"`
}

// ticketSLADTO is a ticket's current SLA state and its cycle history.
type ticketSLADTO struct {
	Current *slaDTO       `json:"current"`
	Cycles  []slaCycleDTO `json:"cycles"`
}

func (a *api) ticketSLA(c *gin.Context) {
	s, ok := a.scope(c)
	if !ok {
		return
	}
	t, cycles, err := a.Tickets.SLAHistory(c.Request.Context(), s, c.Param("t"))
	if err != nil {
		writeError(c, err)
		return
	}
	out := ticketSLADTO{Current: toSLA(t.SLAView), Cycles: []slaCycleDTO{}}
	for _, in := range cycles {
		d := slaCycleDTO{Cycle: in.Cycle, Current: in.Current, Priority: in.Priority,
			Version: in.Snapshot.PolicyVersion, StartedAt: in.StartedAt, ClosedAt: in.ClosedAt, ClosedReason: nullable(in.ClosedReason),
			Targets: []slaClockDTO{}}
		for _, b := range in.Breaches {
			d.Breaches = append(d.Breaches, struct {
				Kind string    `json:"kind"`
				At   time.Time `json:"at"`
			}{string(b.Kind), b.At})
		}
		if d.Breaches == nil {
			d.Breaches = []struct {
				Kind string    `json:"kind"`
				At   time.Time `json:"at"`
			}{}
		}
		for _, cl := range in.Clocks {
			cd := slaClockDTO{Kind: string(cl.Kind), State: string(cl.State), ElapsedSeconds: cl.ElapsedSeconds,
				DueAt: cl.DueAt, WarnAt: cl.WarnAt, MetAt: cl.MetAt, BreachedAt: cl.BreachedAt}
			if cl.BudgetSeconds > 0 {
				b := cl.BudgetSeconds
				cd.BudgetSeconds = &b
			}
			d.Targets = append(d.Targets, cd)
		}
		out.Cycles = append(out.Cycles, d)
	}
	c.JSON(http.StatusOK, out)
}

// ---- Tags ----

// tagDTO is a catalog tag and how many live tickets the caller may read
// carry it.
type tagDTO struct {
	Tag       string    `json:"tag"`
	Count     int64     `json:"count"`
	CreatedAt time.Time `json:"createdAt"`
}

func toTag(t tickets.TagCount) tagDTO {
	return tagDTO{Tag: t.Tag, Count: t.Count, CreatedAt: t.CreatedAt}
}

type createTagRequest struct {
	Name string `json:"name" required:"true" doc:"Normalized like ticket tags: trimmed, lower case, inner spaces become '-'."`
}

func (a *api) listTags(c *gin.Context) {
	s, ok := a.scope(c)
	if !ok {
		return
	}
	rows, err := a.Tickets.Tags(c.Request.Context(), s)
	respond(c, http.StatusOK, items(rows, toTag), err)
}

func (a *api) createTag(c *gin.Context) {
	s, ok := a.scope(c)
	if !ok {
		return
	}
	var body createTagRequest
	if !decode(c, &body) {
		return
	}
	t, err := a.Tickets.CreateTag(c.Request.Context(), s, body.Name)
	respond(c, http.StatusCreated, toTag(t), err)
}

func (a *api) deleteTag(c *gin.Context) {
	s, ok := a.scope(c)
	if !ok {
		return
	}
	_, err := a.Tickets.DeleteTag(c.Request.Context(), s, c.Param("name"))
	respond(c, http.StatusNoContent, nil, err)
}

// ---- List filters ----

// listFilter parses the ad-hoc list filters (comma-separated values).
// It returns nil when none is present.
func listFilter(c *gin.Context) *tickets.Filter {
	split := func(name string) []string {
		raw := strings.TrimSpace(c.Query(name))
		if raw == "" {
			return nil
		}
		parts := strings.Split(raw, ",")
		for i := range parts {
			parts[i] = strings.TrimSpace(parts[i])
		}
		return parts
	}
	f := tickets.Filter{Assignee: c.Query("assignee"), StatusIds: split("statusId"), Tags: split("tag"),
		RequestTypeIds: split("requestTypeId"), OrganisationIds: split("organisationId"), RequesterId: strings.TrimSpace(c.Query("requesterId")), Due: c.Query("due")}
	for _, p := range split("priority") {
		n, err := strconv.Atoi(p)
		if err != nil {
			n = -1 // rejected by validation
		}
		f.Priorities = append(f.Priorities, n)
	}
	if f.Empty() {
		return nil
	}
	return &f
}

func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

// ---- Canned replies (stored and routed as "macros") ----

type macroDTO struct {
	Id         string    `json:"id"`
	ProjectId  string    `json:"projectId"`
	Name       string    `json:"name"`
	Body       string    `json:"body"`
	Visibility string    `json:"visibility" enum:"public,agent"`
	Active     bool      `json:"active"`
	CreatedAt  time.Time `json:"createdAt"`
	UpdatedAt  time.Time `json:"updatedAt"`
	Version    int64     `json:"version"`
}

func toMacro(m macros.Macro) macroDTO {
	return macroDTO{Id: m.Id, ProjectId: m.ProjectId, Name: m.Name, Body: m.Body, Visibility: m.Visibility,
		Active: m.Active, CreatedAt: m.CreatedAt, UpdatedAt: m.UpdatedAt, Version: m.Version}
}

type macroBody struct {
	Name       *string `json:"name"`
	Body       *string `json:"body"`
	Visibility *string `json:"visibility" enum:"public,agent"`
	Active     *bool   `json:"active"`
}

func (b macroBody) input() macros.Input {
	return macros.Input{Name: b.Name, Body: b.Body, Visibility: b.Visibility, Active: b.Active}
}

func (a *api) listMacros(c *gin.Context) {
	s, ok := a.scope(c)
	if !ok {
		return
	}
	rows, err := a.Macros.List(c.Request.Context(), s)
	respond(c, http.StatusOK, items(rows, toMacro), err)
}

func (a *api) createMacro(c *gin.Context) {
	s, ok := a.scope(c)
	if !ok {
		return
	}
	var body macroBody
	if !decode(c, &body) {
		return
	}
	m, err := a.Macros.Create(c.Request.Context(), s, body.input())
	respond(c, http.StatusCreated, toMacro(m), err)
}

func (a *api) getMacro(c *gin.Context) {
	s, ok := a.scope(c)
	if !ok {
		return
	}
	m, err := a.Macros.Get(c.Request.Context(), s, c.Param("m"))
	respond(c, http.StatusOK, toMacro(m), err)
}

func (a *api) patchMacro(c *gin.Context) {
	s, ok := a.scope(c)
	if !ok {
		return
	}
	var body macroBody
	if !decode(c, &body) {
		return
	}
	version, ok := ifMatch(c)
	if !ok {
		return
	}
	m, err := a.Macros.Update(c.Request.Context(), s, c.Param("m"), version, body.input())
	respond(c, http.StatusOK, toMacro(m), err)
}

func (a *api) deleteMacro(c *gin.Context) {
	s, ok := a.scope(c)
	if !ok {
		return
	}
	respond(c, http.StatusNoContent, nil, a.Macros.Delete(c.Request.Context(), s, c.Param("m")))
}

// macroPreviewDTO is a canned reply rendered for one ticket, for the
// composer (nothing is posted).
type macroPreviewDTO struct {
	MacroId    string `json:"macroId"`
	Body       string `json:"body"`
	Visibility string `json:"visibility" enum:"public,agent"`
}

func (a *api) previewMacro(c *gin.Context) {
	s, ok := a.scope(c)
	if !ok {
		return
	}
	p, err := a.Macros.Preview(c.Request.Context(), s, c.Param("t"), c.Param("m"))
	respond(c, http.StatusOK, macroPreviewDTO{MacroId: p.Macro.Id, Body: p.Body, Visibility: p.Visibility}, err)
}

// ---- SLA policy ----

// targetsDTO is one priority's first-response target in wall-clock minutes
// (24/7; null = no target).
type targetsDTO struct {
	FirstResponseMinutes *int `json:"firstResponseMinutes"`
}

// slaSettingsDTO is the deployment's SLA.
type slaSettingsDTO struct {
	Active         bool                  `json:"active" doc:"Whether new tickets get a first-response deadline."`
	WarningPercent int                   `json:"warningPercent" doc:"Tickets are due soon once this share of the target has passed (50–95)."`
	Targets        map[string]targetsDTO `json:"targets" doc:"Keys \"1\" to \"4\" (the priorities)."`
	UpdatedAt      *time.Time            `json:"updatedAt" doc:"Null until the SLA is first saved."`
	Version        int64                 `json:"version" doc:"0 until the SLA is first saved; send it as If-Match."`
}

func optMinutes(m int) *int {
	if m <= 0 {
		return nil
	}
	return &m
}

func toSLASettings(p sla.Policy) slaSettingsDTO {
	out := slaSettingsDTO{Active: p.Active, WarningPercent: p.WarningPercent, Targets: map[string]targetsDTO{}, Version: p.Version}
	if !p.UpdatedAt.IsZero() {
		at := p.UpdatedAt
		out.UpdatedAt = &at
	}
	for i, t := range p.Targets {
		out.Targets[strconv.Itoa(i+1)] = targetsDTO{FirstResponseMinutes: optMinutes(t.FirstResponseMinutes)}
	}
	return out
}

func (a *api) getSLA(c *gin.Context) {
	s, ok := a.scope(c)
	if !ok {
		return
	}
	p, err := a.SLA.Get(c.Request.Context(), s)
	respond(c, http.StatusOK, toSLASettings(p), err)
}

// putSLARequest is the request body of putSLA.
type putSLARequest struct {
	Active         *bool                 `json:"active"`
	WarningPercent *int                  `json:"warningPercent"`
	Targets        map[string]targetsDTO `json:"targets"`
}

func (a *api) putSLA(c *gin.Context) {
	s, ok := a.scope(c)
	if !ok {
		return
	}
	var body putSLARequest
	if !decode(c, &body) {
		return
	}
	// If-Match is the loaded version; omit it or send 0 for the first save.
	var version int64
	if strings.TrimSpace(c.GetHeader("If-Match")) != "" {
		if version, ok = ifMatchZero(c); !ok {
			return
		}
	}
	in := sla.PolicyInput{Active: body.Active == nil || *body.Active, WarningPercent: body.WarningPercent,
		Targets: map[string]sla.TargetsInput{}}
	for p, t := range body.Targets {
		in.Targets[p] = sla.TargetsInput{FirstResponseMinutes: t.FirstResponseMinutes}
	}
	p, err := a.SLA.Put(c.Request.Context(), s, version, in)
	respond(c, http.StatusOK, toSLASettings(p), err)
}

// ---- Reports ----

type countDTO struct {
	Id       *string `json:"id,omitempty"`
	Name     *string `json:"name,omitempty"`
	Category *string `json:"category,omitempty"`
	Priority *int    `json:"priority,omitempty"`
	Count    int64   `json:"count"`
}

func toCounts(cs []reports.Count, priority bool) []countDTO {
	out := make([]countDTO, 0, len(cs))
	for _, c := range cs {
		d := countDTO{Count: c.Count}
		if priority {
			p := c.Priority
			d.Priority = &p
		} else {
			id, name := c.Id, c.Name
			d.Id, d.Name = &id, &name
			if c.Category != "" {
				cat := c.Category
				d.Category = &cat
			}
		}
		out = append(out, d)
	}
	return out
}

type durationsDTO struct {
	MedianSeconds *int64 `json:"medianSeconds"`
	SampleSize    int    `json:"sampleSize"`
	Truncated     bool   `json:"truncated"`
}

type reportDayDTO struct {
	Date     string `json:"date"`
	Created  int64  `json:"created"`
	Resolved int64  `json:"resolved"`
}

type reportRangeDTO struct {
	From     string `json:"from"`
	To       string `json:"to"`
	Timezone string `json:"timezone"`
}

type reportBoundsDTO struct {
	MaxRangeDays int    `json:"maxRangeDays"`
	MaxSamples   int    `json:"maxSamples"`
	Note         string `json:"note"`
}

type reportBacklogDTO struct {
	Total      int64      `json:"total"`
	ByStatus   []countDTO `json:"byStatus"`
	ByPriority []countDTO `json:"byPriority"`
	ByGroup    []countDTO `json:"byGroup"`
}

// reportBreachesDTO counts SLA breaches (first response is the only target).
type reportBreachesDTO struct {
	FirstResponse int64 `json:"firstResponse"`
}

type reportReopenDTO struct {
	Resolved int64    `json:"resolved"`
	Reopened int64    `json:"reopened"`
	Rate     *float64 `json:"rate"`
}

// reportSummaryDTO is the project summary report.
type reportSummaryDTO struct {
	Kind              string            `json:"kind"`
	GeneratedAt       time.Time         `json:"generatedAt"`
	Range             reportRangeDTO    `json:"range"`
	Bounds            reportBoundsDTO   `json:"bounds"`
	Backlog           reportBacklogDTO  `json:"backlog"`
	CreatedVsResolved []reportDayDTO    `json:"createdVsResolved"`
	FirstResponse     durationsDTO      `json:"firstResponse"`
	Resolution        durationsDTO      `json:"resolution"`
	SLABreaches       reportBreachesDTO `json:"slaBreaches"`
	Reopen            reportReopenDTO   `json:"reopen"`
}

func (a *api) reportSummary(c *gin.Context) {
	s, ok := a.scope(c)
	if !ok {
		return
	}
	r, err := a.Reports.Summary(c.Request.Context(), s, c.Query("from"), c.Query("to"))
	if err != nil {
		writeError(c, err)
		return
	}
	days := make([]reportDayDTO, 0, len(r.Days))
	for _, d := range r.Days {
		days = append(days, reportDayDTO{Date: d.Date, Created: d.Created, Resolved: d.Resolved})
	}
	c.JSON(http.StatusOK, reportSummaryDTO{
		Kind:        r.Kind,
		GeneratedAt: r.GeneratedAt,
		Range:       reportRangeDTO{From: r.From, To: r.To, Timezone: "UTC"},
		Bounds: reportBoundsDTO{MaxRangeDays: reports.MaxRangeDays, MaxSamples: reports.MaxSamples,
			Note: "Computed on request from live data (bounded traversal); not a stored point-in-time snapshot. Deleted tickets are excluded."},
		Backlog: reportBacklogDTO{Total: r.Backlog.Total, ByStatus: toCounts(r.Backlog.ByStatus, false),
			ByPriority: toCounts(r.Backlog.ByPriority, true), ByGroup: toCounts(r.Backlog.ByGroup, false)},
		CreatedVsResolved: days,
		FirstResponse:     durationsDTO{MedianSeconds: r.FirstResponse.MedianSeconds, SampleSize: r.FirstResponse.SampleSize, Truncated: r.FirstResponse.Truncated},
		Resolution:        durationsDTO{MedianSeconds: r.Resolution.MedianSeconds, SampleSize: r.Resolution.SampleSize, Truncated: r.Resolution.Truncated},
		SLABreaches:       reportBreachesDTO{FirstResponse: r.SLABreaches},
		Reopen:            reportReopenDTO{Resolved: r.Resolved, Reopened: r.Reopened, Rate: r.ReopenRate},
	})
}
