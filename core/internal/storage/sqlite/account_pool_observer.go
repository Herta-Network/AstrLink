package sqlite

import (
	"context"
	"github.com/QuantumNous/astrlink/core/contract"
	"github.com/QuantumNous/astrlink/core/internal/storage"
)

// SetAccountPoolObserver must be called before the store serves requests.
func (store *Store) SetAccountPoolObserver(observer storage.AccountPoolObserver) {
	store.poolObserver = observer
}

func (store *Store) beginPoolMutation(ctx context.Context, mutation storage.AccountPoolMutation) (func(), error) {
	if store.poolObserver == nil {
		return func() {}, nil
	}
	return store.poolObserver.BeginPoolMutation(ctx, mutation)
}

func (store *Store) notifyAccountPools() {
	if store.poolObserver != nil {
		store.poolObserver.Changed()
	}
}

func (store *Store) GuardServiceDeletion(ctx context.Context, id contract.ServiceID, expected string) (func(), error) {
	release, err := store.beginPoolMutation(ctx, storage.AccountPoolMutation{Exclusive: true, Accounts: []contract.ServiceID{id}})
	if err != nil {
		return nil, err
	}
	failed := true
	defer func() {
		if failed {
			release()
		}
	}()
	current, err := store.GetService(ctx, id)
	if err != nil {
		return nil, err
	}
	if current.ETag != expected {
		return nil, storage.ErrPrecondition
	}
	var pooled bool
	if err := store.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM account_pool_members WHERE service_id = ?)`, id).Scan(&pooled); err != nil {
		return nil, err
	}
	if pooled {
		return nil, storage.ErrPoolMembership
	}
	failed = false
	return release, nil
}
