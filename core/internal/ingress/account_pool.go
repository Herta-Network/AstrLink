package ingress

import (
	"context"
	"errors"

	"github.com/QuantumNous/astrlink/core/contract"
	"github.com/QuantumNous/astrlink/core/internal/accesstoken"
	"github.com/QuantumNous/astrlink/core/internal/accountpool"
	"github.com/QuantumNous/astrlink/core/internal/storage"
)

type requestPoolKey struct{}

func poolForRequest(ctx context.Context) *storage.AccountPoolRecord {
	pool, _ := ctx.Value(requestPoolKey{}).(*storage.AccountPoolRecord)
	return pool
}

func (handler *Handler) bindRequestPool(ctx context.Context) (context.Context, error) {
	principal, ok := accesstoken.PrincipalFromContext(ctx)
	if !ok || principal.RoutingScope == contract.RoutingScopeGlobal {
		return context.WithValue(ctx, requestPoolKey{}, (*storage.AccountPoolRecord)(nil)), nil
	}
	if principal.AccessTokenRouting.Validate() != nil {
		return ctx, accountpool.ErrNotBound
	}
	if handler.accountPools == nil {
		return ctx, accountpool.ErrUnavailable
	}
	pool, err := handler.accountPools.GetAccountPool(ctx, principal.PoolID)
	if errors.Is(err, storage.ErrNotFound) {
		return ctx, accountpool.ErrNotBound
	}
	if err != nil {
		return ctx, accountpool.ErrUnavailable
	}
	ctx = context.WithValue(ctx, requestPoolKey{}, &pool)
	if !pool.Pool.Enabled {
		return ctx, accountpool.ErrUnavailable
	}
	return ctx, nil
}
