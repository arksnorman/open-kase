package access

import "context"

type principalKey struct{}

// WithPrincipal stores the authenticated principal in ctx for adapters that
// hand a request to a non-Gin handler (the MCP endpoint).
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, principalKey{}, p)
}

// PrincipalFrom returns the principal stored by WithPrincipal.
func PrincipalFrom(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(principalKey{}).(Principal)
	return p, ok && p.Id != ""
}
