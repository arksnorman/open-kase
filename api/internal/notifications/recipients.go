package notifications

import (
	"github.com/arksnorman/open-kase/api/internal/access"
	"github.com/arksnorman/open-kase/api/internal/audit"
	"github.com/arksnorman/open-kase/api/internal/outbox"
	"github.com/arksnorman/open-kase/api/templates"
)

// TemplateTicketReceived is the requester's acknowledgement (no link).
const TemplateTicketReceived = templates.KindTicketReceived

// Facts are the inputs of recipient planning, read when the event is
// dispatched. Access, account status and preferences are checked again
// immediately before each email is sent.
type Facts struct {
	Event outbox.Record
	// RequesterAck is the project's "ticket received" setting.
	RequesterAck bool
	Watchers     []string // user IDs watching the ticket
	GroupMembers []string // user IDs in the ticket's current group
	// CC are the ticket's CC addresses: copied on every requester email.
	CC []string
}

// EmailTarget is one planned email.
type EmailTarget struct {
	Kind     string // RecipientUser, RecipientContact or RecipientAddress (a CC address)
	Id       string // user or contact ID, or the CC address
	Reason   string
	Template templates.Kind
}

// Optional reports whether the recipient's preferences may mute this
// email. Requester, assignee and other core notices cannot be muted.
func (t EmailTarget) Optional() bool {
	return t.Reason == ReasonWatcher || t.Reason == ReasonGroupQueue
}

// PlanEmail returns the email recipients of an event (architecture §4.2,
// adapted: requesters are contacts without logins; no customer watchers).
//
//   - ticket.created: requester acknowledgement (project setting) and the
//     ticket's group queue.
//   - public message by staff: requester, assignee, staff watchers.
//   - public message by the requester: assignee and staff watchers; the
//     group queue when the ticket is unassigned.
//   - agent note: link-only notice to the assignee and staff watchers;
//     never to contacts.
//   - claim/release/assignment: the new assignee, or the group queue when
//     the ticket is unassigned (release, transfer).
//   - resolve/archive: requester (not for a resolve made by the requester
//     side); resolve/reopen/archive: assignee and
//     watchers. Other status changes send nothing.
//   - follow-up reminder (automation): the requester only.
//   - every email planned for the requester is also planned, with the same
//     template, for each of the ticket's CC addresses (one delivery each).
//   - scheduled message held for review: its author only (staff notice
//     with a link, no body).
//   - scheduled message cancelled by the system (ticket archived or
//     missing, attachments gone) or an administrator: its author only
//     (same kind of notice); cancelling your own sends nothing.
//
// The author of a change never receives their own notification, and a
// person is planned at most once (core reasons win over optional ones).
func PlanEmail(f Facts) []EmailTarget {
	ev := f.Event
	snap := ev.Snapshot
	actorUser := ""
	if ev.Actor.Type == audit.ActorUser {
		actorUser = ev.Actor.Id
	}
	var out []EmailTarget
	seen := map[string]bool{}
	addUser := func(id, reason string, tpl templates.Kind) {
		if id == "" || id == actorUser || seen["user:"+id] {
			return
		}
		seen["user:"+id] = true
		out = append(out, EmailTarget{Kind: RecipientUser, Id: id, Reason: reason, Template: tpl})
	}
	addRequester := func(tpl templates.Kind) {
		if snap.RequesterId == "" || seen["contact:"+snap.RequesterId] {
			return
		}
		seen["contact:"+snap.RequesterId] = true
		out = append(out, EmailTarget{Kind: RecipientContact, Id: snap.RequesterId, Reason: ReasonRequester, Template: tpl})
		for _, addr := range f.CC {
			if addr == "" || seen["cc:"+addr] {
				continue
			}
			seen["cc:"+addr] = true
			out = append(out, EmailTarget{Kind: RecipientAddress, Id: addr, Reason: ReasonCC, Template: tpl})
		}
	}
	addAll := func(ids []string, reason string, tpl templates.Kind) {
		for _, id := range ids {
			addUser(id, reason, tpl)
		}
	}
	switch ev.Type {
	case outbox.TicketCreated:
		if f.RequesterAck {
			addRequester(TemplateTicketReceived)
		}
		addAll(f.GroupMembers, ReasonGroupQueue, templates.KindTicketCreated)

	case outbox.MessageCreated:
		switch ev.Visibility {
		case "public":
			byRequester := ev.MessageAuthorKind == RecipientContact
			if !byRequester {
				addRequester(templates.KindPublicReply)
			} else {
				// The requester wrote it: suppress the self notification.
				seen["contact:"+ev.MessageAuthorId] = true
			}
			addUser(snap.AssigneeId, ReasonAssignee, templates.KindPublicReply)
			addAll(f.Watchers, ReasonWatcher, templates.KindPublicReply)
			if byRequester && snap.AssigneeId == "" {
				addAll(f.GroupMembers, ReasonGroupQueue, templates.KindPublicReply)
			}
		case "agent":
			// Staff only, link-only notice: contacts are never planned.
			addUser(snap.AssigneeId, ReasonAssignee, templates.KindAgentNoteNotice)
			addAll(f.Watchers, ReasonWatcher, templates.KindAgentNoteNotice)
		}

	case outbox.TicketClaimed, outbox.TicketReleased, outbox.TicketAssigned:
		if snap.AssigneeId != "" {
			addUser(snap.AssigneeId, ReasonAssignee, templates.KindAssignment)
		} else if ev.Type != outbox.TicketClaimed {
			addAll(f.GroupMembers, ReasonGroupQueue, templates.KindAssignment)
		}

	case outbox.TicketFollowupReminder:
		// Lifecycle automation: the requester only (never staff).
		addRequester(templates.KindFollowupReminder)

	case outbox.ScheduledMessageNeedsReview:
		addUser(ev.UserId, ReasonAuthor, templates.KindScheduledReview)

	case outbox.ScheduledMessageCancelled:
		// Otherwise a reply the author counted on silently never goes out
		// (review P-ASYNC-2). The author as actor is skipped by addUser.
		addUser(ev.UserId, ReasonAuthor, templates.KindScheduledCancelled)

	case outbox.TicketSLAWarning, outbox.TicketSLABreached:
		// Staff only: the assignee (essential) and the ticket's group queue
		// (mutable with queueUpdates). Never the requester.
		addUser(snap.AssigneeId, ReasonAssignee, templates.KindSLAAlert)
		addAll(f.GroupMembers, ReasonGroupQueue, templates.KindSLAAlert)

	case outbox.TicketStatusChanged:
		switch ev.Transition {
		case outbox.TransitionResolve, outbox.TransitionArchive:
			// A requester-side resolve is not echoed to the requester or the
			// CC addresses: the customer side did it.
			if ev.Transition != outbox.TransitionResolve || StatusChangedBy(ev) != "requester" {
				addRequester(templates.KindStatusChanged)
			}
			addUser(snap.AssigneeId, ReasonAssignee, templates.KindStatusChanged)
			addAll(f.Watchers, ReasonWatcher, templates.KindStatusChanged)
		case outbox.TransitionReopen:
			addUser(snap.AssigneeId, ReasonAssignee, templates.KindStatusChanged)
			addAll(f.Watchers, ReasonWatcher, templates.KindStatusChanged)
		case outbox.TransitionHoldEnded:
			// The hold ran out: the assignee only (an unassigned ticket just
			// reappears in the queue). The requester is never told about holds.
			addUser(snap.AssigneeId, ReasonAssignee, templates.KindHoldEnded)
		}
	}
	return out
}

// StatusChangedBy says who changed the status: "automation" (auto-resolve,
// auto-archive, hold ended), "requester" (a requester-side token: a resolve,
// or a reply that reopened the ticket) or "staff" (a user or a staff token).
// It is derived from the event's actor and the scopes that allowed the action.
func StatusChangedBy(ev outbox.Record) string {
	switch {
	case ev.Actor.Type == audit.ActorSystem:
		return "automation"
	case ev.Transition == outbox.TransitionRequesterReply,
		ev.Actor.Type == audit.ActorService && len(ev.Actor.Scopes) > 0 && !access.StaffScopes(ev.Actor.Scopes):
		return "requester"
	}
	return "staff"
}
