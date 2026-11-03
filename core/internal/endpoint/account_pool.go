package endpoint

import (
	"context"
	"errors"

	"github.com/QuantumNous/astrlink/core/contract"
	"github.com/QuantumNous/astrlink/core/internal/accesstoken"
	"github.com/QuantumNous/astrlink/core/internal/accountpool"
	"github.com/QuantumNous/astrlink/core/internal/storage"
)

func poolScoped(ctx context.Context) bool {
	principal, ok := accesstoken.PrincipalFromContext(ctx)
	return ok && principal.RoutingScope == contract.RoutingScopePool
}

func (resolver *StoreResolver) scopedPool(ctx context.Context) (*storage.AccountPoolRecord, error) {
	principal, present := accesstoken.PrincipalFromContext(ctx)
	if !present {
		return nil, nil
	}
	if principal.AccessTokenRouting.Validate() != nil {
		return nil, accountpool.ErrNotBound
	}
	if principal.RoutingScope == contract.RoutingScopeGlobal {
		return nil, nil
	}
	pools, ok := resolver.reader.(storage.AccountPoolStore)
	if !ok {
		return nil, accountpool.ErrUnavailable
	}
	record, err := pools.GetAccountPool(ctx, principal.PoolID)
	if errors.Is(err, storage.ErrNotFound) {
		return nil, accountpool.ErrNotBound
	}
	if err != nil || !record.Pool.Enabled {
		return nil, accountpool.ErrUnavailable
	}
	return &record, nil
}
