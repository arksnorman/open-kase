package validation

import (
	"errors"
	"strings"
	"testing"

	"github.com/arksnorman/open-kase/api/package/fault"
)

func fieldErrors(t *testing.T, err error) []fault.FieldError {
	t.Helper()
	var fe *fault.Error
	if !errors.As(err, &fe) || fe.Kind != fault.KindValidation {
		t.Fatalf("expected validation fault, got %v", err)
	}
	return fe.Fields
}

func TestTicketPriority(t *testing.T) {
	for _, p := range []int64{1, 2, 3, 4} {
		if e := TicketPriority.CheckInt("priority", p); e != nil {
			t.Errorf("priority %d rejected: %+v", p, e)
		}
	}
	for _, p := range []int64{0, 5, -1} {
		e := TicketPriority.CheckInt("priority", p)
		if e == nil || e.Code != CodeOutOfRange || e.Message != "Choose Priority 1 to Priority 4." {
			t.Errorf("priority %d: %+v", p, e)
		}
	}
}

func TestProjectKey(t *testing.T) {
	valid := []string{"MAIL", "AB", "X1", "CLOUD2026AB"}
	invalid := []string{"m", "mail", "1MAIL", "MA-IL", "ABCDEFGHIJKLM", "A"}
	for _, k := range valid {
		if e := ProjectKey.CheckString("key", k); e != nil {
			t.Errorf("%q rejected: %+v", k, e)
		}
	}
	for _, k := range invalid {
		if e := ProjectKey.CheckString("key", k); e == nil {
			t.Errorf("%q accepted", k)
		}
	}
}

func TestTicketSubjectCountsCodePoints(t *testing.T) {
	if e := TicketSubject.CheckString("subject", strings.Repeat("é", 200)); e != nil {
		t.Fatalf("200 code points rejected: %+v", e)
	}
	if e := TicketSubject.CheckString("subject", strings.Repeat("é", 201)); e == nil || e.Code != CodeTooLong {
		t.Fatalf("201 code points: %+v", e)
	}
	if e := TicketSubject.CheckString("subject", ""); e == nil || e.Code != CodeRequired {
		t.Fatalf("empty: %+v", e)
	}
	if e := TicketSubject.CheckString("subject", "line\nbreak"); e == nil || e.Code != CodeInvalidChars {
		t.Fatalf("newline accepted in subject: %+v", e)
	}
	if e := MessageBody.CheckString("body", "line\nbreak"); e != nil {
		t.Fatalf("newline rejected in body: %+v", e)
	}
}

func TestEnumsEmailsAndURLs(t *testing.T) {
	if e := MessageVisibility.CheckString("visibility", "private"); e == nil || e.Code != CodeInvalidChoice {
		t.Fatalf("visibility: %+v", e)
	}
	if e := UserEmail.CheckString("email", "Admin <a@example.com>"); e == nil {
		t.Fatal("display-name email accepted")
	}
	if e := UserEmail.CheckString("email", "a@example.com"); e != nil {
		t.Fatalf("valid email rejected: %+v", e)
	}
	if e := SettingsBaseURL.CheckString("baseUrl", "javascript:alert(1)"); e == nil {
		t.Fatal("javascript URL accepted")
	}
	if e := SettingsBaseURL.CheckString("baseUrl", ""); e != nil {
		t.Fatal("optional URL rejected when empty")
	}
	if e := SettingsSMTPHost.CheckString("host", "smtp.example.com"); e != nil {
		t.Fatalf("host rejected: %+v", e)
	}
}

func TestCollector(t *testing.T) {
	var c Collector
	c.String(TicketSubject, "subject", "ok")
	if c.Err() != nil {
		t.Fatal("unexpected error")
	}
	c.Int(TicketPriority, "priority", 0)
	c.String(ProjectKey, "projectKey", "bad")
	errs := fieldErrors(t, c.Err())
	if len(errs) != 2 || errs[0].Field != "priority" || errs[1].Field != "projectKey" {
		t.Fatalf("errors = %+v", errs)
	}
}

func TestCatalogIsContextual(t *testing.T) {
	for _, f := range []string{"ticket.subject", "ticket.priority", "project.key", "project.name", "message.visibility"} {
		if _, ok := Lookup(f); !ok {
			t.Errorf("missing rule %s", f)
		}
	}
	if _, ok := Lookup("name"); ok {
		t.Error("generic 'name' rule must not exist")
	}
	if len(All()) < 10 {
		t.Error("catalog unexpectedly small")
	}
}

type createTicket struct {
	Subject  string `json:"subject"`
	Priority int    `json:"priority"`
	Meta     struct {
		Tag string `json:"tag"`
	} `json:"meta"`
}

func TestDecodeStrict(t *testing.T) {
	var dst createTicket
	if err := DecodeStrict(strings.NewReader(`{"subject":"x","priority":3,"meta":{"tag":"a"}}`), 0, &dst); err != nil {
		t.Fatalf("valid body rejected: %v", err)
	}
	if dst.Priority != 3 || dst.Meta.Tag != "a" {
		t.Fatalf("decoded %+v", dst)
	}

	cases := map[string]struct {
		body string
		kind fault.Kind
		code string
	}{
		"unknown field":   {`{"subject":"x","assigneeId":"u"}`, fault.KindValidation, "validationFailed"},
		"operator key":    {`{"subject":{"$ne":null}}`, fault.KindValidation, "validationFailed"},
		"nested operator": {`{"meta":{"$where":"1"}}`, fault.KindValidation, "validationFailed"},
		"dotted key":      {`{"meta.tag":"x"}`, fault.KindValidation, "validationFailed"},
		"wrong type":      {`{"priority":"high"}`, fault.KindValidation, "validationFailed"},
		"malformed":       {`{"subject":`, fault.KindMalformed, "malformedJson"},
		"trailing":        {`{"subject":"x"}{"subject":"y"}`, fault.KindMalformed, "malformedJson"},
		"too large":       {`{"subject":"` + strings.Repeat("a", 200) + `"}`, fault.KindTooLarge, "bodyTooLarge"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			var d createTicket
			err := DecodeStrict(strings.NewReader(tc.body), 100, &d)
			fe := fault.As(err)
			if err == nil || fe.Kind != tc.kind || fe.Code != tc.code {
				t.Fatalf("got %v, want kind %s code %s", err, tc.kind, tc.code)
			}
		})
	}
}

func TestDecodeStrictAllowsDollarInValues(t *testing.T) {
	var d createTicket
	if err := DecodeStrict(strings.NewReader(`{"subject":"$100 refund. Why?"}`), 0, &d); err != nil {
		t.Fatalf("value containing $ and . rejected: %v", err)
	}
}

func TestIdentityAndCustomerRules(t *testing.T) {
	for _, ok := range []string{"system", "light", "dark"} {
		if UserTheme.CheckString("theme", ok) != nil {
			t.Errorf("theme %s rejected", ok)
		}
	}
	if UserTheme.CheckString("theme", "") == nil || UserTheme.CheckString("theme", "neon") == nil {
		t.Error("invalid theme accepted")
	}
	if IntegrationScope.CheckString("s", "admin:all") == nil || IntegrationScope.CheckString("s", "tickets:read") != nil {
		t.Error("integration scope rule")
	}
	if ContactExternalID.CheckString("externalId", "crm 42") == nil || ContactExternalID.CheckString("externalId", "crm:42/a") != nil {
		t.Error("external id rule")
	}
	if !ValidID("0123456789abcdef01234567") || ValidID("0123456789ABCDEF01234567") || ValidID("MAIL") {
		t.Error("ValidID")
	}
	var c Collector
	empty, name := "", "Ops"
	c.OptionalString(GroupName, "name", nil)
	c.OptionalString(GroupName, "name", &name)
	c.OptionalString(GroupDescription, "description", &empty)
	if c.Err() != nil {
		t.Fatalf("optional fields: %v", c.Err())
	}
	c.OptionalString(GroupName, "name", &empty)
	c.OptionalString(UserStatus, "status", &empty)
	if errs := fieldErrors(t, c.Err()); len(errs) != 2 || errs[0].Code != CodeRequired {
		t.Fatalf("empty required patch fields: %+v", errs)
	}
}
