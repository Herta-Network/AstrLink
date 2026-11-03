package sync

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/astrlink/core/contract"
	"github.com/QuantumNous/astrlink/core/internal/accesstoken"
	"github.com/QuantumNous/astrlink/core/internal/secretstore"
	"github.com/QuantumNous/astrlink/core/internal/storage"
)

func createPoolSyncFixture(t *testing.T, pair *pair) (storage.AccountPoolRecord, accesstoken.CreatedToken) {
	t.Helper()
	ctx := context.Background()
	for _, device := range []*device{pair.first, pair.second} {
		config := pair.syncConfig()
		config.Scope.Tokens = true
		if err := device.engine.Configure(ctx, config); err != nil {
			t.Fatal(err)
		}
		if err := device.engine.SetPassphrase(ctx, []byte("pool sync test passphrase")); err != nil {
			t.Fatal(err)
		}
	}
	service := contract.Service{ID: "service_pool_sync", Name: "Account", Kind: contract.ServiceKindKiroSubscription, Enabled: true, Models: []string{"model"}, Capabilities: contract.SubscriptionProviderKiro.Capabilities(), Subscription: &contract.SubscriptionConnection{Provider: contract.SubscriptionProviderKiro, Status: contract.SubscriptionStatusDisconnected}}
	if _, err := pair.first.store.CreateService(ctx, service, storage.CredentialMutation{}); err != nil {
		t.Fatal(err)
	}
	pool := contract.DefaultAccountPool()
	pool.ID, pool.Name, pool.Provider = "pool_sync_a", "A", contract.SubscriptionProviderKiro
	pool.Proxy = &contract.ServiceProxy{Mode: "custom", URL: "socks5://proxy.example:1080"}
	record, err := pair.first.store.CreateAccountPool(ctx, pool, storage.PoolProxyMutation{Present: true, Credential: &contract.ProxyCredential{Username: "proxy-user", Password: "proxy-password"}})
	if err != nil {
		t.Fatal(err)
	}
	record, err = pair.first.store.ReplaceAccountPoolMembers(ctx, pool.ID, []contract.AccountPoolMember{{PoolID: pool.ID, ServiceID: service.ID, Enabled: true, MaxConcurrentRequests: 2}}, record.ETag)
	if err != nil {
		t.Fatal(err)
	}
	manager, _ := accesstoken.NewManager(pair.first.store)
	key, err := manager.Create(ctx, "pooled", contract.AccessTokenRouting{RoutingScope: contract.RoutingScopePool, PoolID: pool.ID})
	if err != nil {
		t.Fatal(err)
	}
	return record, key
}

func TestPoolSyncPreservesMembersProxyAndTokenScope(t *testing.T) {
	pair := newPair(t)
	pool, key := createPoolSyncFixture(t, pair)
	ctx := context.Background()
	pair.first.clock = time.Now().UTC().Add(time.Second)
	pair.sync(pair.first)
	document, _, _ := remotePackage(t, pair.first)
	if document.Version != 3 {
		t.Fatalf("pool package version=%d", document.Version)
	}
	pair.second.clock = time.Now().UTC().Add(2 * time.Second)
	pair.sync(pair.second)
	imported, err := pair.second.store.GetAccountPool(ctx, pool.Pool.ID)
	if err != nil || len(imported.Members) != 1 || imported.Members[0].ServiceID != pool.Members[0].ServiceID {
		t.Fatalf("pool import: %+v %v", imported, err)
	}
	credential, err := pair.second.store.Get(ctx, secretstore.Ref(imported.Pool.Proxy.CredentialRef))
	if err != nil || !strings.Contains(string(credential), "proxy-password") {
		t.Fatalf("pool proxy import: %v", err)
	}
	clear(credential)
	manager, _ := accesstoken.NewManager(pair.second.store)
	principal, err := manager.AuthenticatePrincipal(ctx, key.Value)
	if err != nil || principal.RoutingScope != contract.RoutingScopePool || principal.PoolID != pool.Pool.ID {
		t.Fatalf("scoped token became global: %+v %v", principal, err)
	}
	// A coherent transfer must not depend on Go map iteration ordering.
	pool, err = pair.first.store.ReplaceAccountPoolMembers(ctx, pool.Pool.ID, []contract.AccountPoolMember{}, pool.ETag)
	if err != nil {
		t.Fatal(err)
	}
	next := contract.DefaultAccountPool()
	next.ID, next.Name, next.Provider = "pool_sync_b", "B", contract.SubscriptionProviderKiro
	replacement, err := pair.first.store.CreateAccountPool(ctx, next, storage.PoolProxyMutation{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = pair.first.store.ReplaceAccountPoolMembers(ctx, next.ID, []contract.AccountPoolMember{{PoolID: next.ID, ServiceID: "service_pool_sync", Enabled: true, MaxConcurrentRequests: 3}}, replacement.ETag)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pair.first.store.UpdateAccessTokenRouting(ctx, key.Token.ID, contract.AccessTokenRouting{RoutingScope: contract.RoutingScopePool, PoolID: next.ID}, key.Token.ETag()); err != nil {
		t.Fatal(err)
	}
	if err := pair.first.store.DeleteAccountPool(ctx, pool.Pool.ID, pool.ETag); err != nil {
		t.Fatal(err)
	}
	pair.first.clock = time.Now().UTC().Add(3 * time.Second)
	pair.sync(pair.first)
	pair.second.clock = time.Now().UTC().Add(4 * time.Second)
	pair.sync(pair.second)
	owner, found, err := pair.second.store.AccountPoolForService(ctx, "service_pool_sync")
	if err != nil || !found || owner.Pool.ID != next.ID {
		t.Fatalf("transfer failed: %+v %t %v", owner, found, err)
	}
	principal, err = manager.AuthenticatePrincipal(ctx, key.Value)
	if err != nil || principal.PoolID != next.ID {
		t.Fatalf("token did not follow explicit rebind: %+v %v", principal, err)
	}
}

func TestPoolTokenImportRejectsMissingDependencies(t *testing.T) {
	pair := newPair(t)
	_, key := createPoolSyncFixture(t, pair)
	ctx := context.Background()
	config := pair.syncConfig()
	config.Scope.Tokens = true
	config.Scope.Services = false
	if err := pair.second.engine.Configure(ctx, config); err != nil {
		t.Fatal(err)
	}
	pair.first.clock = time.Now().UTC().Add(time.Second)
	pair.sync(pair.first)
	if _, err := pair.second.engine.Sync(ctx); err == nil {
		t.Fatal("pool token imported without its pool")
	}
	manager, _ := accesstoken.NewManager(pair.second.store)
	if principal, err := manager.AuthenticatePrincipal(ctx, key.Value); err == nil {
		t.Fatalf("unresolved pool token authenticated: %+v", principal)
	}
}

func TestSyncScopeVersionDoesNotGuessGlobalPermissions(t *testing.T) {
	entity := Entity{Document: json.RawMessage(`{"name":"key","source":"user","created_at":"2026-11-03T00:00:00Z"}`), Secret: "unused"}
	old := NewDocument("device", 0, time.Now())
	old.Version = 2
	old.Put(KindAccessToken, "token_a", entity)
	raw, _ := old.Encode()
	decoded, err := Decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	var token contract.AccessTokenDocument
	if err := json.Unmarshal(decoded.Entities[KindAccessToken]["token_a"].Document, &token); err != nil || token.RoutingScope != contract.RoutingScopeGlobal {
		t.Fatal("published global token was not upgraded", err)
	}
	old.Version = 3
	raw, _ = old.Encode()
	if _, err := Decode(raw); err == nil {
		t.Fatal("v3 missing scope silently became global")
	}
	old.Version = 2
	entity.Document = json.RawMessage(`{"name":"key","source":"user","routing_scope":"pool","pool_id":"pool_a"}`)
	old.Put(KindAccessToken, "token_a", entity)
	raw, _ = old.Encode()
	if _, err := Decode(raw); err == nil {
		t.Fatal("scoped token admitted under an old writer version")
	}
}
