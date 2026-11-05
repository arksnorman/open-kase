package templates

import (
	"errors"
	"io/fs"
	"strings"
	"testing"
	"time"

	"github.com/arksnorman/open-kase/api/package/product"
)

func renderer(t *testing.T) *Renderer {
	t.Helper()
	r, err := NewRenderer()
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func staffTicket() Ticket {
	return Ticket{Ref: "#7", Subject: "Payslip missing", URL: "https://support.example.com/tickets/abc"}
}

func allKinds() []Data {
	return []Data{
		TicketCreatedData{Ticket: staffTicket(), GroupName: "Triage", RequesterName: "Ada", Priority: 2},
		PublicReplyData{Ticket: staffTicket(), AuthorName: "Grace", Body: "We fixed it."},
		AgentNoteNoticeData{Ticket: staffTicket(), AuthorName: "Grace"},
		AssignmentData{Ticket: staffTicket(), GroupName: "Payroll L2", AssigneeName: "Grace", ChangedByName: "Lin"},
		StatusChangedData{Ticket: staffTicket(), Transition: TransitionResolved, StatusName: "Resolved"},
		TestEmailData{RequestedBy: "Admin", SentAt: time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC)},
		TicketReceivedData{Ticket: Ticket{Ref: "#7", Subject: "Payslip missing"}, Common: Common{RecipientName: "Ada"}},
		SLAAlertData{Ticket: staffTicket(), Target: "firstResponse", Breached: true, DueAt: time.Date(2026, 10, 2, 9, 30, 0, 0, time.UTC),
			Priority: 1, GroupName: "Triage"},
		FollowupReminderData{Ticket: Ticket{Ref: "#7", Subject: "Payslip missing"}, Common: Common{RecipientName: "Ada"},
			AutoResolveNotice: true},
		ScheduledReviewData{Ticket: staffTicket(), Reason: "newerRequesterReply", Visibility: "public"},
		ScheduledCancelledData{Ticket: staffTicket(), Reason: "ticketArchived", Visibility: "agent"},
		RequesterReplyData{Ticket: Ticket{Ref: "#7", Subject: "Payslip missing"}, Common: Common{RecipientName: "Ada"}},
		HoldEndedData{Ticket: staffTicket(), Reason: "Waiting on Engineering"},
		PasswordResetData{Common: Common{RecipientName: "Ada"}, URL: "https://support.example.com/reset-password?token=abc",
			ExpiresAt: time.Date(2026, 10, 2, 9, 30, 0, 0, time.UTC)},
		InvitationData{URL: "https://support.example.com/accept-invitation?token=abc", InvitedBy: "Lin", Email: "ada@example.com",
			ExpiresAt: time.Date(2026, 10, 5, 9, 30, 0, 0, time.UTC)},
	}
}

func TestAccountLinksHaveTextAlternativeAndExpiry(t *testing.T) {
	r := renderer(t)
	for _, d := range allKinds()[len(allKinds())-2:] {
		out, err := r.Render(d)
		if err != nil {
			t.Fatal(err)
		}
		for name, body := range map[string]string{"text": out.Text, "html": out.HTML} {
			if !strings.Contains(body, "token=abc") || !strings.Contains(body, "expires at 2026-10-0") {
				t.Errorf("%s %s: link or expiry missing:\n%s", d.Kind(), name, body)
			}
		}
	}
	if _, err := r.Render(PasswordResetData{URL: "javascript:alert(1)", ExpiresAt: time.Now()}); !errors.Is(err, ErrInvalidData) {
		t.Fatalf("unsafe URL accepted: %v", err)
	}
}

func TestEveryKindRendersWithFooter(t *testing.T) {
	r := renderer(t)
	seen := map[Kind]bool{}
	for _, d := range allKinds() {
		out, err := r.Render(d)
		if err != nil {
			t.Fatalf("%s: %v", d.Kind(), err)
		}
		seen[d.Kind()] = true
		if out.TemplateId != string(d.Kind())+".v1" || out.Version != 1 {
			t.Errorf("%s: template id %q", d.Kind(), out.TemplateId)
		}
		if out.Subject == "" || strings.ContainsAny(out.Subject, "\r\n") {
			t.Errorf("%s: subject %q", d.Kind(), out.Subject)
		}
		for name, body := range map[string]string{"text": out.Text, "html": out.HTML} {
			if !strings.Contains(body, "Replies to this email are not processed") {
				t.Errorf("%s %s: missing no-reply notice", d.Kind(), name)
			}
			if !strings.Contains(body, product.Name) {
				t.Errorf("%s %s: missing product name", d.Kind(), name)
			}
			if strings.Contains(body, "<no value>") {
				t.Errorf("%s %s: unresolved field", d.Kind(), name)
			}
		}
		if strings.Contains(out.Text, "<p") {
			t.Errorf("%s: html in text body", d.Kind())
		}
	}
	for k := range Versions {
		if !seen[k] {
			t.Errorf("kind %s not covered", k)
		}
	}
}

func TestEveryVersionedTemplateFileIsEmbedded(t *testing.T) {
	files, err := fs.Glob(FS, "email/*")
	if err != nil {
		t.Fatal(err)
	}
	have := map[string]bool{}
	for _, f := range files {
		have[f] = true
	}
	for k, v := range Versions {
		for _, suffix := range []string{".subject.txt", ".txt", ".html"} {
			name := "email/" + string(k) + ".v" + string(rune('0'+v)) + suffix
			if !have[name] {
				t.Errorf("missing %s", name)
			}
		}
	}
}

func TestUntrustedFieldsAreEscaped(t *testing.T) {
	r := renderer(t)
	tk := staffTicket()
	tk.Subject = `<script>alert(1)</script>` + "\r\nBcc: victim@example.com"
	out, err := r.Render(PublicReplyData{
		Ticket:     tk,
		AuthorName: `"><img src=x onerror=alert(1)>`,
		Body:       "<b>bold</b> & <a href=\"javascript:alert(1)\">x</a>\n\nline two\u202e",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"<script>", "<img", "<b>bold", `<a href="javascript`} {
		if strings.Contains(out.HTML, bad) {
			t.Errorf("html contains %q", bad)
		}
	}
	if !strings.Contains(out.HTML, "&lt;b&gt;bold&lt;/b&gt;") {
		t.Errorf("body not escaped:\n%s", out.HTML)
	}
	if strings.ContainsAny(out.Subject, "\r\n") || !strings.Contains(out.Subject, "Bcc: victim") {
		t.Errorf("subject = %q", out.Subject)
	}
	if strings.ContainsRune(out.Text, '\u202e') || strings.ContainsRune(out.HTML, '\u202e') {
		t.Error("bidi override kept")
	}
	if !strings.Contains(out.Text, "<b>bold</b>") {
		t.Errorf("text body should carry the literal text:\n%s", out.Text)
	}
}

// requesterKinds returns every requester-facing kind for one ticket reference.
func requesterKinds(tk Ticket) []Data {
	c := Common{RecipientName: "Ada", ProductName: "Help Desk"}
	return []Data{
		RequesterReplyData{Common: c, Ticket: tk},
		TicketReceivedData{Common: c, Ticket: tk},
		FollowupReminderData{Common: c, Ticket: tk, AutoResolveNotice: true},
		StatusChangedData{Common: c, Ticket: tk, Transition: TransitionResolved, StatusName: "Resolved"},
	}
}

func TestRequesterEmailsWithoutLinkPointToTheSubmittingSystem(t *testing.T) {
	r := renderer(t)
	tk := Ticket{Ref: "#7", Subject: "Payslip missing"}
	for _, d := range requesterKinds(tk) {
		out, err := r.Render(d)
		if err != nil {
			t.Fatalf("%s: %v", d.Kind(), err)
		}
		for _, body := range []string{out.Text, out.HTML} {
			if strings.Contains(body, "Open the ticket") || strings.Contains(body, "href=") {
				t.Errorf("%s: requester email without a configured link has a link:\n%s", d.Kind(), body)
			}
			if !strings.Contains(body, "open the system you used to submit it") || !strings.Contains(body, "#7") ||
				!strings.Contains(body, "Please do not reply to this email") {
				t.Errorf("%s: missing guidance:\n%s", d.Kind(), body)
			}
			if !strings.Contains(body, "Help Desk") || strings.Contains(body, product.Name) {
				t.Errorf("%s: configured product name not used:\n%s", d.Kind(), body)
			}
			low := strings.ToLower(body)
			if strings.Contains(low, "reply to this email to") || strings.Contains(low, "reply above this line") {
				t.Errorf("%s: implies email replies are processed:\n%s", d.Kind(), body)
			}
		}
	}
}

func TestRequesterEmailsLinkToTheConfiguredURL(t *testing.T) {
	r := renderer(t)
	tk := Ticket{Ref: "#7", Subject: "Payslip missing", URL: "https://portal.example.org/requests/7"}
	for _, d := range requesterKinds(tk) {
		out, err := r.Render(d)
		if err != nil {
			t.Fatalf("%s: %v", d.Kind(), err)
		}
		for _, body := range []string{out.Text, out.HTML} {
			if !strings.Contains(body, "https://portal.example.org/requests/7") {
				t.Errorf("%s: link missing:\n%s", d.Kind(), body)
			}
		}
	}
}

func TestRequesterReplyHasNoContent(t *testing.T) {
	out, err := renderer(t).Render(RequesterReplyData{Ticket: Ticket{Ref: "#7", Subject: "Payslip missing"}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.Text, "You received a response on ticket #7") || !strings.Contains(out.Subject, "#7") {
		t.Fatalf("text = %s", out.Text)
	}
}

func TestPublicReplyIsTruncated(t *testing.T) {
	out, err := renderer(t).Render(PublicReplyData{Ticket: staffTicket(), Body: strings.Repeat("a", MaxBodyPreviewRunes+50)})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.Text, strings.Repeat("a", MaxBodyPreviewRunes)) || !strings.Contains(out.Text, "This reply was shortened") {
		t.Fatal("long body not truncated")
	}
}

func TestAgentNoteNoticeIsLinkOnly(t *testing.T) {
	out, err := renderer(t).Render(AgentNoteNoticeData{Ticket: staffTicket(), AuthorName: "Grace"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.Text, "https://support.example.com/tickets/abc") || !strings.Contains(out.Text, "The note is not") {
		t.Fatalf("text = %s", out.Text)
	}
	tk := staffTicket()
	tk.URL = ""
	if _, err := renderer(t).Render(AgentNoteNoticeData{Ticket: tk}); !errors.Is(err, ErrInvalidData) {
		t.Fatalf("missing URL accepted: %v", err)
	}
}

func TestAssignmentVariants(t *testing.T) {
	r := renderer(t)
	toPerson, err := r.Render(AssignmentData{Ticket: staffTicket(), GroupName: "L2", AssigneeName: "Grace"})
	if err != nil {
		t.Fatal(err)
	}
	toQueue, err := r.Render(AssignmentData{Ticket: staffTicket(), GroupName: "L2"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(toPerson.Subject, "Assigned to Grace") || !strings.Contains(toQueue.Subject, "Moved to L2") {
		t.Fatalf("subjects %q / %q", toPerson.Subject, toQueue.Subject)
	}
	if !strings.Contains(toQueue.Text, "waiting unassigned") {
		t.Fatalf("queue text = %s", toQueue.Text)
	}
}

func TestStatusTransitions(t *testing.T) {
	r := renderer(t)
	for tr, want := range map[string]string{
		TransitionResolved: "Ticket resolved",
		TransitionReopened: "Ticket reopened",
		TransitionArchived: "Ticket archived",
	} {
		out, err := r.Render(StatusChangedData{Ticket: staffTicket(), Transition: tr})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out.Subject, want) || !strings.Contains(out.HTML, want) {
			t.Errorf("%s: %q", tr, out.Subject)
		}
	}
	if _, err := r.Render(StatusChangedData{Ticket: staffTicket(), Transition: "deleted"}); !errors.Is(err, ErrInvalidData) {
		t.Fatalf("unknown transition: %v", err)
	}
}

func TestInvalidData(t *testing.T) {
	r := renderer(t)
	bad := map[string]Data{
		"priority 0":        TicketCreatedData{Ticket: staffTicket(), GroupName: "T", Priority: 0},
		"priority 5":        TicketCreatedData{Ticket: staffTicket(), GroupName: "T", Priority: 5},
		"no group":          TicketCreatedData{Ticket: staffTicket(), Priority: 3},
		"javascript url":    AgentNoteNoticeData{Ticket: Ticket{Ref: "#1", Subject: "s", URL: "javascript:alert(1)"}},
		"url with userinfo": AgentNoteNoticeData{Ticket: Ticket{Ref: "#1", Subject: "s", URL: "https://u:p@x.example/"}},
		"empty reply":       PublicReplyData{Ticket: staffTicket(), Body: " \n "},
		"no key":            PublicReplyData{Ticket: Ticket{Subject: "s"}, Body: "b"},
		"no sent time":      TestEmailData{},
	}
	for name, d := range bad {
		if _, err := r.Render(d); !errors.Is(err, ErrInvalidData) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}

func TestTicketCreatedShowsPriorityLabel(t *testing.T) {
	out, err := renderer(t).Render(TicketCreatedData{Ticket: staffTicket(), GroupName: "Triage", Priority: 1})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.Text, "Critical") || !strings.Contains(out.HTML, "Critical") {
		t.Fatalf("text = %s", out.Text)
	}
}

func TestLineAndPreview(t *testing.T) {
	if got := Line("  a\r\n\tb\u2028c\x00d\u202e "); got != "a b c d" {
		t.Errorf("Line = %q", got)
	}
	if got := Line(strings.Repeat("x", 500)); len([]rune(got)) != MaxLineRunes {
		t.Errorf("Line length = %d", len([]rune(got)))
	}
	if got, cut := Preview("a\r\nb\x07\rc", 100); got != "a\nb\nc" || cut {
		t.Errorf("Preview = %q %v", got, cut)
	}
	if got, cut := Preview("abcdef", 4); got != "abc…" || !cut {
		t.Errorf("Preview cut = %q %v", got, cut)
	}
}
