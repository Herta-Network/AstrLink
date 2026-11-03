package accountpool

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/QuantumNous/astrlink/core/contract"
	"github.com/QuantumNous/astrlink/core/internal/accesstoken"
	"github.com/QuantumNous/astrlink/core/internal/storage"
	"github.com/QuantumNous/astrlink/core/internal/storage/sqlite"
)

func runtimeFixture(t *testing.T) (*Runtime, *sqlite.Store, storage.AccountPoolRecord, storage.AccessTokenMetadata) {
	t.Helper()
	ctx := context.Background()
	store, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "runtime.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	service := contract.Service{ID: "service_a", Name: "A", Kind: contract.ServiceKindKiroSubscription, Enabled: true, Models: []string{"m"}, Capabilities: contract.SubscriptionProviderKiro.Capabilities(), Subscription: &contract.SubscriptionConnection{Provider: contract.SubscriptionProviderKiro, Status: contract.SubscriptionStatusDisconnected}}
	if _, err := store.CreateService(ctx, service, storage.CredentialMutation{}); err != nil {
		t.Fatal(err)
	}
	pool := contract.DefaultAccountPool()
	pool.ID, pool.Name, pool.Provider = "pool_a", "A", contract.SubscriptionProviderKiro
	record, err := store.CreateAccountPool(ctx, pool, storage.PoolProxyMutation{})
	if err != nil {
		t.Fatal(err)
	}
	record, err = store.ReplaceAccountPoolMembers(ctx, pool.ID, []contract.AccountPoolMember{{PoolID: pool.ID, ServiceID: service.ID, Enabled: true, MaxConcurrentRequests: 1}}, record.ETag)
	if err != nil {
		t.Fatal(err)
	}
	manager, _ := accesstoken.NewManager(store)
	created, err := manager.Create(ctx, "pool", contract.AccessTokenRouting{RoutingScope: contract.RoutingScopePool, PoolID: pool.ID})
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := NewRuntime(ctx, store)
	if err != nil {
		t.Fatal(err)
	}
	return runtime, store, record, created.Token
}

func TestRuntimePreservesWeeklyCooldownUntilNewCompleteEvidence(t *testing.T) {
	runtime, store, _, _ := runtimeFixture(t)
	ctx := context.Background()
	now := time.Now().UTC()
	short, week := now.Add(time.Hour), now.Add(48*time.Hour)
	usage := contract.SubscriptionUsage{FetchedAt: now, Primary: &contract.RateLimitWindow{UsedPercent: 100, ResetAt: &short}, Secondary: &contract.RateLimitWindow{UsedPercent: 100, ResetAt: &week}}
	if err := runtime.ObserveUsage(ctx, "service_a", usage); err != nil {
		t.Fatal(err)
	}
	stored, err := store.ListAccountQuotaCooldowns(ctx)
	if err != nil || len(stored) != 1 || len(stored[0].Windows) != 2 || !stored[0].RetryAt().Equal(week) {
		t.Fatalf("cooldown: %+v %v", stored, err)
	}
	restarted, err := NewRuntime(ctx, store)
	if err != nil {
		t.Fatal(err)
	}
	usage.FetchedAt = now.Add(time.Minute)
	usage.Primary = &contract.RateLimitWindow{UsedPercent: 1}
	usage.Secondary = nil
	if err := restarted.ObserveUsage(ctx, "service_a", usage); err != nil {
		t.Fatal(err)
	}
	if _, cooling := restarted.Scheduler.Cooldown("service_a"); !cooling {
		t.Fatal("missing weekly window cleared cooldown")
	}
	usage.Secondary = &contract.RateLimitWindow{UsedPercent: 1}
	usage.FetchedAt = now.Add(-time.Minute)
	if err := restarted.ObserveUsage(ctx, "service_a", usage); err != nil {
		t.Fatal(err)
	}
	if _, cooling := restarted.Scheduler.Cooldown("service_a"); !cooling {
		t.Fatal("stale snapshot cleared cooldown")
	}
	usage.FetchedAt = now.Add(2 * time.Minute)
	if err := restarted.ObserveUsage(ctx, "service_a", usage); err != nil {
		t.Fatal(err)
	}
	if _, cooling := restarted.Scheduler.Cooldown("service_a"); cooling {
		t.Fatal("fresh complete quota did not restore account")
	}
	stored, err = store.ListAccountQuotaCooldowns(ctx)
	if err != nil || len(stored) != 0 {
		t.Fatal("recovery was not durable", err)
	}
}

func TestKeyRebindingWakesAndRejectsQueuedRequest(t *testing.T) {
	runtime, store, _, principal := runtimeFixture(t)
	load := func(ctx context.Context) (View, error) {
		return runtime.View(ctx, principal, []contract.ServiceID{"service_a"}, false)
	}
	held, err := runtime.Scheduler.Acquire(context.Background(), Request{Load: load})
	if err != nil {
		t.Fatal(err)
	}
	defer held.Finish(false)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		permit, err := runtime.Scheduler.Acquire(ctx, Request{Load: load})
		if permit != nil {
			permit.Finish(false)
		}
		done <- err
	}()
	waitForQueued(t, runtime.Scheduler, 1)
	if _, err := store.UpdateAccessTokenRouting(ctx, principal.ID, contract.AccessTokenRouting{RoutingScope: contract.RoutingScopeGlobal}, principal.ETag()); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, ErrChanged) {
			t.Fatalf("rebound request was not rejected: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("rebind did not wake waiter")
	}
	waitForQueued(t, runtime.Scheduler, 0)
}

func TestPoolMutationCannotRemoveRunningAccount(t *testing.T) {
	runtime, store, pool, principal := runtimeFixture(t)
	ctx := context.Background()
	permit, err := runtime.Scheduler.Acquire(ctx, Request{Load: func(ctx context.Context) (View, error) {
		return runtime.View(ctx, principal, []contract.ServiceID{"service_a"}, false)
	}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReplaceAccountPoolMembers(ctx, pool.Pool.ID, []contract.AccountPoolMember{}, pool.ETag); !errors.Is(err, storage.ErrConflict) {
		t.Fatalf("running account removed: %v", err)
	}
	permit.Finish(false)
	if _, err := store.ReplaceAccountPoolMembers(ctx, pool.Pool.ID, []contract.AccountPoolMember{}, pool.ETag); err != nil {
		t.Fatal(err)
	}
}

func TestRuntimeMergesIndependentUpstreamWindowsAndRejectsEqualTimestampRecovery(t *testing.T) {
	runtime, store, _, _ := runtimeFixture(t)
	ctx := context.Background()
	short, week := time.Now().Add(time.Hour), time.Now().Add(48*time.Hour)
	for _, window := range []contract.AccountQuotaWindow{{ID: "primary", ResetAt: &short}, {ID: "secondary", ResetAt: &week}} {
		handled, err := runtime.ObserveExhaustion(ctx, "service_a", []contract.AccountQuotaWindow{window})
		if err != nil || !handled {
			t.Fatalf("observe: %v %v", handled, err)
		}
	}
	current, ok := runtime.Scheduler.Cooldown("service_a")
	if !ok || len(current.Windows) != 2 || !current.RetryAt().Equal(week) {
		t.Fatalf("merged cooldown: %+v", current)
	}
	usage := contract.SubscriptionUsage{FetchedAt: current.ObservedAt, Primary: &contract.RateLimitWindow{UsedPercent: 1}, Secondary: &contract.RateLimitWindow{UsedPercent: 1}}
	if err := runtime.ObserveUsage(ctx, "service_a", usage); err != nil {
		t.Fatal(err)
	}
	if _, cooling := runtime.Scheduler.Cooldown("service_a"); !cooling {
		t.Fatal("equal-time observation cleared newer exhaustion")
	}
	stored, err := store.ListAccountQuotaCooldowns(ctx)
	if err != nil || len(stored) != 1 || len(stored[0].Windows) != 2 {
		t.Fatalf("stored windows: %+v %v", stored, err)
	}
}
