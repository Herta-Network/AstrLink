package contract

import (
	"testing"
	"time"
)

func TestPoolRoutingRequiresExplicitScope(t *testing.T) {
	for _, routing := range []AccessTokenRouting{
		{}, {RoutingScope: RoutingScopePool}, {RoutingScope: RoutingScopeGlobal, PoolID: "pool_a"}, {RoutingScope: "other"},
	} {
		if routing.Validate() == nil {
			t.Fatalf("invalid scope accepted: %+v", routing)
		}
	}
	for _, routing := range []AccessTokenRouting{{RoutingScope: RoutingScopeGlobal}, {RoutingScope: RoutingScopePool, PoolID: "pool_a"}} {
		if err := routing.Validate(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestPoolMembershipDoesNotEnableClaudeCLI(t *testing.T) {
	pool := DefaultAccountPool()
	pool.ID, pool.Name, pool.Provider = "pool_claude", "Claude", SubscriptionProviderClaudeCode
	if err := pool.Validate(); err != nil {
		t.Fatal(err)
	}
	service := Service{Kind: ServiceKindClaudeSubscription, Subscription: &SubscriptionConnection{Provider: SubscriptionProviderClaudeCode}}
	for _, mode := range []SubscriptionMode{"", SubscriptionModeLegacy} {
		service.Subscription.Mode = mode
		if err := pool.ValidateMember(service); err != nil {
			t.Fatal(err)
		}
	}
	service.Subscription.Mode = SubscriptionModeOfficial
	if pool.ValidateMember(service) == nil {
		t.Fatal("CLI account entered pool")
	}
	service.Kind, service.Subscription.Provider = ServiceKindKiroSubscription, SubscriptionProviderKiro
	if pool.ValidateMember(service) == nil {
		t.Fatal("different provider entered pool")
	}
}

func TestPoolQuotaWaitsForEveryExhaustedWindow(t *testing.T) {
	now := time.Now().UTC()
	short, weekly := now.Add(time.Hour), now.Add(48*time.Hour)
	cooldown := AccountQuotaCooldown{ServiceID: "service_a", ObservedAt: now, Source: "usage", Windows: []AccountQuotaWindow{{ID: "five_hour", ResetAt: &short}, {ID: "weekly", ResetAt: &weekly}}}
	if err := cooldown.Validate(); err != nil {
		t.Fatal(err)
	}
	if !cooldown.RetryAt().Equal(weekly) {
		t.Fatal("short reset incorrectly restored exhausted week")
	}
	cooldown.Windows[1].ResetAt = nil
	if !cooldown.RetryAt().IsZero() {
		t.Fatal("unknown reset was guessed")
	}
}
