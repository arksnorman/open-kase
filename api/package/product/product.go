// Package product holds the single definition of the product's public
// identity. Rename the product by editing this file only.
package product

// Name is the human-readable product name.
const Name = "open-kase"

// Binary is the CLI/executable name.
const Binary = "open-kase"

// Slug is a lowercase identifier used for URNs, database defaults and
// environment-variable prefixes.
const Slug = "open-kase"

// EnvPrefix prefixes product-specific bootstrap environment variables.
const EnvPrefix = "OPEN_KASE_"

// DefaultDatabase is used when the MongoDB URI does not name a database.
const DefaultDatabase = "open_kase"

// Repository is the canonical source repository.
const Repository = "https://github.com/arksnorman/open-kase"

// TokenPrefix starts every API token so secret scanners can recognise it.
const TokenPrefix = "kase_live_"

// HeaderPrefix starts the product's custom HTTP and email header names.
const HeaderPrefix = "X-Open-Kase-"

// CookiePrefix starts the product's cookie names.
const CookiePrefix = "kase_"

// Build metadata, overridden at link time with -ldflags -X.
var (
	Version = "0.0.0-dev"
	Commit  = "unknown"
	BuiltAt = "unknown"
)

// ErrorTypeURN returns the problem-details type URN for an error kind.
func ErrorTypeURN(kind string) string {
	return "urn:" + Slug + ":error:" + kind
}
