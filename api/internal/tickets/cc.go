package tickets

import (
	"slices"
	"strconv"
	"strings"

	"github.com/arksnorman/open-kase/api/package/validation"
)

// MaxCCEmails bounds the CC addresses of one ticket.
const MaxCCEmails = 10

// NormalizeCCEmails trims and lower-cases the CC addresses, drops
// duplicates (keeping the first position) and validates each address and
// the count. requesterEmail, when known, may not be in the list: the
// requester already receives every requester email.
func NormalizeCCEmails(v *validation.Collector, field string, in []string, requesterEmail string) []string {
	out := make([]string, 0, len(in))
	req := normalizeAddress(requesterEmail)
	for i, raw := range in {
		e := normalizeAddress(raw)
		f := field + "[" + strconv.Itoa(i) + "]"
		if fe := validation.TicketCCEmail.CheckString(f, e); fe != nil {
			v.Add(fe.Field, fe.Code, fe.Message)
			continue
		}
		if req != "" && e == req {
			v.Add(f, "requesterEmail", "The requester already receives these emails; remove their address from CC.")
			continue
		}
		if !slices.Contains(out, e) {
			out = append(out, e)
		}
	}
	if len(out) > MaxCCEmails {
		v.Int(validation.TicketCCCount, field, int64(len(out)))
	}
	return out
}

// normalizeAddress is the stored form of a CC address (trimmed, lower
// case); addresses are compared in this form.
func normalizeAddress(a string) string {
	return strings.ToLower(strings.TrimSpace(a))
}
