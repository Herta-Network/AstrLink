package subscription

import (
	"context"
	"errors"
	"time"

	"github.com/QuantumNous/astrlink/core/contract"
	"github.com/QuantumNous/astrlink/core/internal/accountauth"
)

var ErrKiroImportBusy = errors.New("another authorization is in progress")

// ImportKiro validates the grant with the provider and serializes its commit
// with sign-in, logout and refresh for the same service.
func (manager *Manager) ImportKiro(ctx context.Context, id contract.ServiceID, grant contract.KiroSocialGrant, save func(accountauth.AccountTokens) error) error {
	manager.mu.Lock()
	_, authorizing := manager.authorizationAttempts[id]
	if manager.lifecycleTransitions[id] || authorizing {
		manager.mu.Unlock()
		return ErrKiroImportBusy
	}
	manager.lifecycleTransitions[id] = true
	manager.mu.Unlock()
	defer manager.endLifecycleTransition(id)

	manager.kiroTokens.Invalidate(id)
	defer manager.kiroTokens.Activate(id)
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	config := manager.kiroConfig
	tokens, err := accountauth.NewTokenClient(config).RefreshAccount(ctx, accountauth.AccountTokens{
		KiroAuthMethod: "social", Region: "us-east-1", RefreshToken: grant.RefreshToken,
	})
	if err != nil {
		return err
	}
	if err := accountauth.EnsureKiroProfile(ctx, config, &tokens); err != nil {
		return err
	}
	if err := save(tokens); err != nil {
		return err
	}
	manager.mu.Lock()
	delete(manager.usageCache, id)
	delete(manager.forcedRefreshAt, id)
	manager.mu.Unlock()
	return nil
}
