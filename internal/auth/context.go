package auth

import "context"

// ctxKey is unexported so no other package can collide with or forge the
// identity slot.
type ctxKey struct{}

// WithIdentity returns a context carrying the authenticated caller.
func WithIdentity(ctx context.Context, id Identity) context.Context {
	return context.WithValue(ctx, ctxKey{}, id)
}

// FromContext returns the authenticated caller, if any. The zero Identity
// with ok=false means the request was never authenticated — an exempt path,
// or a router running with auth disabled. Callers must treat that as
// "unattributed", never as "trusted".
func FromContext(ctx context.Context) (Identity, bool) {
	id, ok := ctx.Value(ctxKey{}).(Identity)
	return id, ok
}

// PrincipalFromContext is the reqlog convenience: principal and token id, or
// two empty strings when the request carried no identity.
func PrincipalFromContext(ctx context.Context) (principal, tokenID string) {
	if id, ok := FromContext(ctx); ok {
		return id.Principal, id.TokenID
	}
	return "", ""
}
