package http

import (
	"encoding/json"

	"github.com/arksnorman/open-kase/api/internal/customers"
	"github.com/arksnorman/open-kase/api/internal/macros"
	"github.com/arksnorman/open-kase/api/internal/projects"
	"github.com/arksnorman/open-kase/api/internal/statuses"
	"github.com/arksnorman/open-kase/api/internal/tickets"
)

// The view functions expose the REST representations to the MCP adapter,
// so both transports return exactly the same (already audience-projected)
// fields.

// TicketView returns the contract's Ticket for t.
func TicketView(t tickets.Ticket) any { return toTicket(t) }

// MessageView returns the contract's Message for m.
func MessageView(m tickets.Message) any { return toMessage(m) }

// GroupView returns the contract's Group for g.
func GroupView(g projects.Group) any { return toGroup(g) }

// MemberView returns the contract's Member (directory entry) for m.
func MemberView(m projects.Member) any { return toMember(m) }

// ContactView returns the contract's Contact for c.
func ContactView(c customers.Contact) any { return toContact(c) }

// MacroView returns the contract's Macro (canned reply) for m.
func MacroView(m macros.Macro) any { return toMacro(m) }

// TagView returns the contract's Tag for t.
func TagView(t tickets.TagCount) any { return toTag(t) }

// StatusView returns the contract's Status for s.
func StatusView(s statuses.Status) any { return toStatus(s) }

// SearchView returns the contract's SearchPage for r as a JSON object.
func SearchView(r tickets.SearchResult) map[string]any {
	raw, _ := json.Marshal(toSearchPage(r))
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return out
}
