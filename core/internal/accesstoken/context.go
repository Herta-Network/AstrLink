package accesstoken

import "context"

type principalKey struct{}

// WithPrincipal carries authenticated, server-owned routing scope. No HTTP
// request header or body field is used to populate it.
func WithPrincipal(ctx context.Context, token Token) context.Context {
	return context.WithValue(ctx, principalKey{}, token)
}

func PrincipalFromContext(ctx context.Context) (Token, bool) {
	token, ok := ctx.Value(principalKey{}).(Token)
	return token, ok
}
