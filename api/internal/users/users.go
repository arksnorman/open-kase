// Package users manages staff identities (agents and administrators). Only
// these users sign in; requesters are contacts, not users.
package users

import (
	"slices"
	"strconv"
	"strings"
	"time"
)

// RoleAdmin is the stored role of administrators. Users without it are
// agents.
const RoleAdmin = "admin"

// Account status values.
const (
	StatusActive   = "active"
	StatusDisabled = "disabled"
	StatusInvited  = "invited" // created without a password; cannot sign in yet
)

// Theme preferences.
const (
	ThemeSystem = "system"
	ThemeLight  = "light"
	ThemeDark   = "dark"
)

// NormalizeEmail returns the canonical form used for uniqueness and lookup.
func NormalizeEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

// NormalizeUsername returns the canonical form of a username (trimmed,
// lower case). The result is validated with validation.UserUsername.
func NormalizeUsername(username string) string {
	return strings.ToLower(strings.TrimSpace(username))
}

// IsEmailIdentifier reports whether a sign-in identifier is an email
// address rather than a username (usernames never contain "@").
func IsEmailIdentifier(login string) bool { return strings.Contains(login, "@") }

// Username length bounds (validation.UserUsername).
const (
	usernameMin = 3
	usernameMax = 32
)

// UsernameBase derives a username candidate from an email address: the
// local part, lower-cased, with characters outside [a-z0-9._-] removed and
// leading non-letters stripped; shorter than 3 characters it is prefixed
// with "user", longer than 32 it is cut. The result always satisfies
// validation.UserUsername.
func UsernameBase(email string) string {
	local := NormalizeEmail(email)
	if i := strings.LastIndexByte(local, '@'); i >= 0 {
		local = local[:i]
	}
	var b strings.Builder
	for _, c := range local {
		switch {
		case c >= 'a' && c <= 'z':
			b.WriteRune(c)
		case b.Len() == 0:
			// A username starts with a letter.
		case (c >= '0' && c <= '9') || c == '.' || c == '_' || c == '-':
			b.WriteRune(c)
		}
	}
	base := b.String()
	if len(base) < usernameMin {
		if base == "" {
			base = "user"
		} else {
			base = "user-" + base
		}
	}
	if len(base) > usernameMax {
		base = base[:usernameMax]
	}
	return base
}

// UsernameCandidate returns base for n <= 1 and base with the numeric suffix
// n otherwise ("dana2"), cutting base so the result stays within 32
// characters.
func UsernameCandidate(base string, n int) string {
	if n <= 1 {
		return base
	}
	suffix := strconv.Itoa(n)
	if len(base)+len(suffix) > usernameMax {
		base = base[:usernameMax-len(suffix)]
	}
	return base + suffix
}

// User is a staff identity.
type User struct {
	Id              string
	WorkspaceId     string
	Email           string
	EmailNormalized string
	// Username is a second sign-in identifier, unique per workspace and
	// stored normalized (validation.UserUsername).
	Username string
	// Name is the display name, derived from the name parts (SetName) and
	// stored alongside them for denormalized reads.
	Name         string
	GivenName    string
	MiddleName   string
	FamilyName   string
	PasswordHash string
	Status       string
	Roles        []string
	Theme        string
	// Signature is the optional Markdown reply signature (user.signature).
	Signature         string
	EmailPrefs        EmailPreferences
	PasswordChangedAt *time.Time
	// MFAEnabledAt is set while TOTP multi-factor authentication is enabled.
	MFAEnabledAt *time.Time
	CreatedAt    time.Time
	UpdatedAt    time.Time
	Version      int64
}

// PersonName returns the user's name parts.
func (u User) PersonName() PersonName {
	return PersonName{Given: u.GivenName, Middle: u.MiddleName, Family: u.FamilyName}
}

// SetName stores the name parts and the derived display name.
func (u *User) SetName(n PersonName) {
	u.GivenName, u.MiddleName, u.FamilyName, u.Name = n.Given, n.Middle, n.Family, n.Full()
}

// IsAdmin reports whether the user has the admin role.
func (u User) IsAdmin() bool { return slices.Contains(u.Roles, RoleAdmin) }

// MFAEnabled reports whether the user has confirmed a TOTP authenticator.
func (u User) MFAEnabled() bool { return u.MFAEnabledAt != nil }

// Active reports whether the user may sign in and use the API.
func (u User) Active() bool { return u.Status == StatusActive }

// EffectiveTheme returns the stored theme or the default.
func (u User) EffectiveTheme() string {
	if u.Theme == "" {
		return ThemeSystem
	}
	return u.Theme
}

// EmailPreferences are a user's optional notification email choices. Core
// notices (tickets assigned to the user, replies on their tickets) cannot
// be muted; watcher updates and new-ticket queue notices can.
type EmailPreferences struct {
	MuteWatcherUpdates bool
	MuteQueueUpdates   bool
}
