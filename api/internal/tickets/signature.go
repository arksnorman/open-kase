package tickets

import (
	"context"
	"regexp"
	"strings"

	"github.com/arksnorman/open-kase/api/internal/access"
	"github.com/arksnorman/open-kase/api/package/fault"
	"github.com/arksnorman/open-kase/api/package/validation"
)

// Signatures supplies the Markdown signatures the server appends to staff
// public replies: the author's own and the workspace's.
type Signatures interface {
	Signatures(ctx context.Context, workspaceID, userID string) (personal, workspace string, err error)
}

// withSignatures returns body followed by the personal and then the
// workspace signature, each after a blank line. Empty signatures are
// skipped.
func withSignatures(body string, sigs ...string) string {
	for _, sig := range sigs {
		if sig = strings.TrimSpace(sig); sig != "" {
			// An unclosed code fence would swallow the signature; a blank
			// line keeps the previous last line from becoming a setext heading.
			body = closeFences(strings.TrimRight(body, " \t\r\n")) + "\n\n" + sig
		}
	}
	return body
}

var fenceLine = regexp.MustCompile("^ {0,3}(`{3,}|~{3,})(.*)$")

// closeFences appends the closing fence of a code block left open at the
// end of md.
func closeFences(md string) string {
	open := ""
	for _, line := range strings.Split(md, "\n") {
		m := fenceLine.FindStringSubmatch(strings.TrimRight(line, "\r"))
		if m == nil {
			continue
		}
		switch {
		case open == "":
			if m[1][0] == '`' && strings.Contains(m[2], "`") {
				continue // inline code, not a fence
			}
			open = m[1]
		case m[1][0] == open[0] && len(m[1]) >= len(open) && strings.TrimSpace(m[2]) == "":
			open = ""
		}
	}
	if open == "" {
		return md
	}
	return md + "\n" + open
}

// checkSigned rejects a signed body that no longer fits the message limits.
func (s *Service) checkSigned(original, signed string) error {
	if signed == original {
		return nil
	}
	var v validation.Collector
	s.bodyRule(&v, "body", signed)
	if v.Err() != nil {
		return fault.Field("body", validation.CodeTooLong, "The reply plus your signature is too long.")
	}
	return nil
}

// signed appends the signatures for a message authored by principal p.
// Only signed-in users carry a signature; integration tokens never do.
func (s *Service) signed(ctx context.Context, p access.Principal, body string) (string, error) {
	if s.signatures == nil || !p.IsUser() {
		return body, nil
	}
	personal, workspace, err := s.signatures.Signatures(ctx, p.WorkspaceId, p.Id)
	if err != nil {
		return "", err
	}
	return withSignatures(body, personal, workspace), nil
}
