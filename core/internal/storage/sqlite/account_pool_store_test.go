package sqlite

import (
	"context"
	"crypto/rand"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/astrlink/core/contract"
	"github.com/QuantumNous/astrlink/core/internal/accesstoken"
	"github.com/QuantumNous/astrlink/core/internal/secretstore"
	"github.com/QuantumNous/astrlink/core/internal/storage"
)

func poolTestAccount(t *testing.T, store *Store, id contract.ServiceID, provider contract.SubscriptionProvider, mode contract.SubscriptionMode) storage.ServiceRecord {
	t.Helper()
	service := contract.Service{ID: id, Name: string(id), Enabled: true, Kind: provider.ServiceKind(), Models: []string{"model"}, Capabilities: provider.Capabilities(), Subscription: &contract.SubscriptionConnection{Provider: provider, Mode: mode, Status: contract.SubscriptionStatusDisconnected}}
	record, err := store.CreateService(context.Background(), service, storage.CredentialMutation{})
	if err != nil {
		t.Fatal(err)
	}
	return record
}

func poolTestCreate(t *testing.T, store *Store, id contract.AccountPoolID, provider contract.SubscriptionProvider) storage.AccountPoolRecord {
	t.Helper()
	pool := contract.DefaultAccountPool()
	pool.ID, pool.Name, pool.Provider = id, string(id), provider
	record, err := store.CreateAccountPool(context.Background(), pool, storage.PoolProxyMutation{})
	if err != nil {
		t.Fatal(err)
	}
	return record
}

func TestAccountPoolMembershipIsAtomicAndProviderScoped(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t, filepath.Join(t.TempDir(), "pools.db"))
	defer store.Close()
	a := poolTestAccount(t, store, "service_a", contract.SubscriptionProviderClaudeCode, contract.SubscriptionModeLegacy)
	b := poolTestAccount(t, store, "service_b", contract.SubscriptionProviderClaudeCode, contract.SubscriptionModeLegacy)
	cli := poolTestAccount(t, store, "service_cli", contract.SubscriptionProviderClaudeCode, contract.SubscriptionModeOfficial)
	kiro := poolTestAccount(t, store, "service_kiro", contract.SubscriptionProviderKiro, "")
	p := poolTestCreate(t, store, "pool_a", contract.SubscriptionProviderClaudeCode)
	q := poolTestCreate(t, store, "pool_b", contract.SubscriptionProviderClaudeCode)
	member := func(pool contract.AccountPoolID, service contract.ServiceID) contract.AccountPoolMember {
		return contract.AccountPoolMember{PoolID: pool, ServiceID: service, Enabled: true, MaxConcurrentRequests: 2}
	}
	oldTag := p.ETag
	var err error
	p, err = store.ReplaceAccountPoolMembers(ctx, p.Pool.ID, []contract.AccountPoolMember{member(p.Pool.ID, a.Service.ID)}, p.ETag)
	if err != nil {
		t.Fatal(err)
	}
	if p.ETag == oldTag {
		t.Fatal("membership did not change ETag")
	}
	if _, err := store.UpdateAccountPool(ctx, p.Pool, storage.PoolProxyMutation{}, oldTag); !errors.Is(err, storage.ErrPrecondition) {
		t.Fatalf("stale tag accepted: %v", err)
	}
	if _, err := store.ReplaceAccountPoolMembers(ctx, q.Pool.ID, []contract.AccountPoolMember{member(q.Pool.ID, b.Service.ID)}, q.ETag); err != nil {
		t.Fatal(err)
	}
	for _, other := range []contract.ServiceID{b.Service.ID, cli.Service.ID, kiro.Service.ID, "service_missing"} {
		if _, err := store.ReplaceAccountPoolMembers(ctx, p.Pool.ID, []contract.AccountPoolMember{member(p.Pool.ID, a.Service.ID), member(p.Pool.ID, other)}, p.ETag); err == nil {
			t.Fatalf("invalid member %s accepted", other)
		}
		current, err := store.GetAccountPool(ctx, p.Pool.ID)
		if err != nil || current.ETag != p.ETag || len(current.Members) != 1 {
			t.Fatalf("failed update was not atomic: %+v %v", current, err)
		}
	}
	changedMode := a.Service
	connection := *changedMode.Subscription
	connection.Mode = contract.SubscriptionModeOfficial
	changedMode.Subscription = &connection
	if _, err := store.UpdateService(ctx, changedMode, storage.CredentialMutation{}, a.ETag); !errors.Is(err, storage.ErrPoolMembership) {
		t.Fatalf("mode change did not identify pool membership: %v", err)
	}
	if err := store.DeleteService(ctx, a.Service.ID, a.ETag); !errors.Is(err, storage.ErrPoolMembership) {
		t.Fatalf("pooled service delete: %v", err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE services SET document_json = json_set(document_json, '$.subscription.mode', 'official') WHERE id = ?`, a.Service.ID); err == nil {
		t.Fatal("SQL update converted pooled account to CLI")
	}
	if _, err := store.db.ExecContext(ctx, `INSERT INTO account_pool_members (pool_id, service_id, enabled, max_concurrent_requests) VALUES (?, ?, 1, 2)`, p.Pool.ID, cli.Service.ID); err == nil {
		t.Fatal("SQL import admitted CLI account")
	}
	current, ok, err := store.AccountPoolForService(ctx, a.Service.ID)
	if err != nil || !ok || current.Pool.ID != p.Pool.ID {
		t.Fatalf("membership lookup: %+v %t %v", current, ok, err)
	}
}

func TestAccountPoolProxyIsSealedAndSurvivesRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "pools.db")
	store := openTestStore(t, path)
	pool := contract.DefaultAccountPool()
	pool.ID, pool.Name, pool.Provider = "pool_a", "Pool", contract.SubscriptionProviderKiro
	pool.Proxy = &contract.ServiceProxy{Mode: "custom", URL: "socks5://proxy.example:1080"}
	record, err := store.CreateAccountPool(ctx, pool, storage.PoolProxyMutation{Present: true, Credential: &contract.ProxyCredential{Username: "pool-user", Password: "pool-password"}})
	if err != nil {
		t.Fatal(err)
	}
	var stored []byte
	if err := store.db.QueryRowContext(ctx, `SELECT credential_value FROM account_pool_proxy_credentials WHERE pool_id = ?`, pool.ID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(stored), "pool-password") {
		t.Fatal("proxy password was stored in plaintext")
	}
	store.Close()
	store = openTestStore(t, path)
	defer store.Close()
	current, err := store.GetAccountPool(ctx, pool.ID)
	if err != nil || current.ETag != record.ETag {
		t.Fatalf("pool restart: %+v %v", current, err)
	}
	secret, err := store.Get(ctx, secretstore.Ref(current.Pool.Proxy.CredentialRef))
	if err != nil || !strings.Contains(string(secret), "pool-password") {
		t.Fatal("proxy credential did not survive", err)
	}
	clear(secret)
	current.Pool.Name = "Renamed"
	updated, err := store.UpdateAccountPool(ctx, current.Pool, storage.PoolProxyMutation{}, current.ETag)
	if err != nil || updated.Pool.Proxy.CredentialRef == "" {
		t.Fatal("ordinary update lost credential", err)
	}
	updated.Pool.Proxy = &contract.ServiceProxy{Mode: "direct"}
	if _, err := store.UpdateAccountPool(ctx, updated.Pool, storage.PoolProxyMutation{}, updated.ETag); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(ctx, secretstore.Ref(record.Pool.Proxy.CredentialRef)); !errors.Is(err, secretstore.ErrNotFound) {
		t.Fatalf("direct mode kept unused credential: %v", err)
	}
}

func TestAccountQuotaCooldownRejectsStaleRecovery(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t, filepath.Join(t.TempDir(), "cooldowns.db"))
	defer store.Close()
	account := poolTestAccount(t, store, "service_a", contract.SubscriptionProviderKiro, "")
	now := time.Now().UTC()
	old := contract.AccountQuotaCooldown{ServiceID: account.Service.ID, Source: "usage", ObservedAt: now, Windows: []contract.AccountQuotaWindow{{ID: "credits"}}}
	if err := store.PutAccountQuotaCooldown(ctx, old); err != nil {
		t.Fatal(err)
	}
	newer := old
	newer.ObservedAt = now.Add(time.Second)
	if err := store.PutAccountQuotaCooldown(ctx, newer); err != nil {
		t.Fatal(err)
	}
	if err := store.PutAccountQuotaCooldown(ctx, old); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteAccountQuotaCooldown(ctx, old.ServiceID, old); err != nil {
		t.Fatal(err)
	}
	list, err := store.ListAccountQuotaCooldowns(ctx)
	if err != nil || len(list) != 1 || !list[0].ObservedAt.Equal(newer.ObservedAt) {
		t.Fatalf("stale recovery cleared cooldown: %+v %v", list, err)
	}
	if err := store.DeleteAccountQuotaCooldown(ctx, newer.ServiceID, newer); err != nil {
		t.Fatal(err)
	}
	list, err = store.ListAccountQuotaCooldowns(ctx)
	if err != nil || len(list) != 0 {
		t.Fatalf("current recovery failed: %+v %v", list, err)
	}
}

func TestPoolDatabaseAndKeyRestoreKeepsScopesSecretsBindingsAndCooldown(t *testing.T) {
	ctx := context.Background()
	directory := t.TempDir()
	source := filepath.Join(directory, "source.db")
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	defer clear(key)
	store, err := Open(ctx, source, WithLocalKey(key))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	account := poolTestAccount(t, store, "service_restore", contract.SubscriptionProviderKiro, "")
	pool := contract.DefaultAccountPool()
	pool.ID, pool.Name, pool.Provider = "pool_restore", "Restore", contract.SubscriptionProviderKiro
	pool.Proxy = &contract.ServiceProxy{Mode: "custom", URL: "socks5://127.0.0.1:1080"}
	record, err := store.CreateAccountPool(ctx, pool, storage.PoolProxyMutation{Present: true, Credential: &contract.ProxyCredential{Username: "restore-user", Password: "restore-secret"}})
	if err != nil {
		t.Fatal(err)
	}
	record, err = store.ReplaceAccountPoolMembers(ctx, pool.ID, []contract.AccountPoolMember{{PoolID: pool.ID, ServiceID: account.Service.ID, Enabled: true, MaxConcurrentRequests: 2}}, record.ETag)
	if err != nil {
		t.Fatal(err)
	}
	tokens, err := accesstoken.NewManager(store)
	if err != nil {
		t.Fatal(err)
	}
	token, err := tokens.Create(ctx, "Restore key", contract.AccessTokenRouting{RoutingScope: contract.RoutingScopePool, PoolID: pool.ID})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	scope := contract.ChannelBindingScope{SessionID: "session_restore", Principal: string(token.Token.ID), PoolID: pool.ID, Protocol: contract.ProtocolAnthropicMessages, Model: "model"}
	binding := contract.ChannelBinding{ChannelBindingScope: scope, ServiceID: account.Service.ID, Source: "explicit", RequestID: "request_restore", UpdatedAt: now, ExpiresAt: now.Add(time.Hour)}
	if err := store.RememberChannelBinding(ctx, binding, now); err != nil {
		t.Fatal(err)
	}
	cooldown := contract.AccountQuotaCooldown{ServiceID: account.Service.ID, ObservedAt: now, Source: "upstream", Windows: []contract.AccountQuotaWindow{{ID: "account"}}}
	if err := store.PutAccountQuotaCooldown(ctx, cooldown); err != nil {
		t.Fatal(err)
	}
	backup := filepath.Join(directory, "restored.db")
	// SQLite's snapshot includes committed WAL state, without copying live
	// pages or requiring a production profile.
	if _, err := store.db.ExecContext(ctx, `VACUUM INTO ?`, backup); err != nil {
		t.Fatal(err)
	}
	restored, err := Open(ctx, backup, WithLocalKey(key))
	if err != nil {
		t.Fatal(err)
	}
	current, err := restored.GetAccountPool(ctx, pool.ID)
	if err != nil || current.ETag != record.ETag || len(current.Members) != 1 {
		t.Fatalf("pool restore: %+v %v", current, err)
	}
	secret, err := restored.Get(ctx, secretstore.Ref(current.Pool.Proxy.CredentialRef))
	if err != nil || !strings.Contains(string(secret), "restore-secret") {
		t.Fatal("proxy credential did not restore", err)
	}
	clear(secret)
	restoredTokens, err := accesstoken.NewManager(restored)
	if err != nil {
		t.Fatal(err)
	}
	principal, err := restoredTokens.AuthenticatePrincipal(ctx, token.Value)
	if err != nil || principal.AccessTokenRouting != token.Token.AccessTokenRouting {
		t.Fatalf("key scope restore: %+v %v", principal, err)
	}
	got, found, err := restored.GetChannelBinding(ctx, scope)
	if err != nil || !found || got.ServiceID != account.Service.ID {
		t.Fatalf("binding restore: %+v %v %v", got, found, err)
	}
	cooldowns, err := restored.ListAccountQuotaCooldowns(ctx)
	if err != nil || len(cooldowns) != 1 || cooldowns[0].ServiceID != account.Service.ID {
		t.Fatalf("cooldown restore: %+v %v", cooldowns, err)
	}
	if err := restored.Close(); err != nil {
		t.Fatal(err)
	}
	wrong := make([]byte, 32)
	if _, err := rand.Read(wrong); err != nil {
		t.Fatal(err)
	}
	defer clear(wrong)
	if invalid, err := Open(ctx, backup, WithLocalKey(wrong), WithExistingDatabase()); !errors.Is(err, ErrLocalKeyMismatch) {
		if invalid != nil {
			invalid.Close()
		}
		t.Fatalf("backup without its key was accepted: %v", err)
	}
}
