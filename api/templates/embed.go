// Package templates embeds the versioned email templates and renders them.
//
// Each email kind has three files under email/: <kind>.v<N>.subject.txt,
// <kind>.v<N>.txt (plain text, text/template) and <kind>.v<N>.html
// (html/template, auto-escaped), plus the shared layout.v1 files. A changed
// template gets a new version file instead of being edited in place, so
// delivery history can name the exact template that produced a message.
//
// Templates receive an authorized, minimal view built for one recipient:
// callers never pass a whole ticket. Agent-note notices have no body field
// at all, so a private note cannot leak through a template mistake.
package templates

import "embed"

// FS holds the email templates.
//
//go:embed email/*.html email/*.txt
var FS embed.FS
