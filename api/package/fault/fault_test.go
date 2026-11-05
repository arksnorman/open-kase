package fault

import (
	"errors"
	"fmt"
	"net/http"
	"testing"
)

func TestToProblemValidation(t *testing.T) {
	err := Validation(FieldError{Field: "priority", Code: "outOfRange", Message: "Choose Priority 1 to Priority 4."})
	p := ToProblem(fmt.Errorf("create ticket: %w", err), "req-1")
	if p.Status != http.StatusUnprocessableEntity || p.Code != "validationFailed" || p.RequestId != "req-1" {
		t.Fatalf("unexpected problem: %+v", p)
	}
	if p.Type != "urn:open-kase:error:validation" {
		t.Fatalf("type = %s", p.Type)
	}
	if len(p.Errors) != 1 || p.Errors[0].Code != "outOfRange" {
		t.Fatalf("errors = %+v", p.Errors)
	}
}

func TestToProblemHidesInternalCause(t *testing.T) {
	p := ToProblem(errors.New("mongo: connection refused at 10.0.0.5"), "r")
	if p.Status != http.StatusInternalServerError || p.Code != "internalError" || p.Detail != "" {
		t.Fatalf("internal details leaked: %+v", p)
	}
}

func TestConflictStatus(t *testing.T) {
	p := ToProblem(Conflict("ticketAlreadyClaimed", "Another agent claimed this ticket."), "")
	if p.Status != http.StatusConflict || p.Detail == "" {
		t.Fatalf("unexpected problem: %+v", p)
	}
}
