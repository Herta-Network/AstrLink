package subscription

import (
	"context"
	"errors"

	"github.com/QuantumNous/astrlink/core/contract"
	"github.com/QuantumNous/astrlink/core/internal/storage"
)

// StorageAccountStore adapts the persistent storage contract to AccountStore.
type StorageAccountStore struct {
	Store storage.SubscriptionAccountStore
}

func (adapter StorageAccountStore) ListAccounts(ctx context.Context) ([]contract.SubscriptionAccount, error) {
	return adapter.Store.ListSubscriptionAccounts(ctx)
}

func (adapter StorageAccountStore) GetAccount(ctx context.Context, id contract.SubscriptionAccountID) (contract.SubscriptionAccount, error) {
	account, err := adapter.Store.GetSubscriptionAccount(ctx, id)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return contract.SubscriptionAccount{}, ErrNotFound
		}
		return contract.SubscriptionAccount{}, err
	}
	return account, nil
}

func (adapter StorageAccountStore) PutAccount(ctx context.Context, account contract.SubscriptionAccount) error {
	return adapter.Store.PutSubscriptionAccount(ctx, account)
}

func (adapter StorageAccountStore) DeleteAccount(ctx context.Context, id contract.SubscriptionAccountID) error {
	if err := adapter.Store.DeleteSubscriptionAccount(ctx, id); err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return ErrNotFound
		}
		return err
	}
	return nil
}

func (adapter StorageAccountStore) GuardDeleteAccount(ctx context.Context, id contract.ServiceID) (func(), error) {
	guard, ok := adapter.Store.(storage.ServiceDeletionGuard)
	if !ok {
		return func() {}, nil
	}
	services, ok := adapter.Store.(storage.ServiceStore)
	if !ok {
		return func() {}, nil
	}
	current, err := services.GetService(ctx, id)
	if err != nil {
		return nil, err
	}
	return guard.GuardServiceDeletion(ctx, id, current.ETag)
}
