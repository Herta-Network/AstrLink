package contract

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

type AccountPoolID string

func (id AccountPoolID) Validate() error { return validateResourceID("account pool", string(id)) }

const (
	DefaultAccountConcurrency = 2
	MaxAccountConcurrency     = 1024
	DefaultPoolWaiting        = 32
	MaxPoolWaiting            = 128
	DefaultPoolWaitMS         = 5000
)

type AccountPoolQueue struct {
	MaxWaitingRequests int `json:"max_waiting_requests"`
	TimeoutMS          int `json:"timeout_ms"`
}

func (queue *AccountPoolQueue) UnmarshalJSON(data []byte) error {
	type document AccountPoolQueue
	var value document
	if err := decodePolicy(data, &value, []string{"max_waiting_requests", "timeout_ms"}, nil); err != nil {
		return err
	}
	if err := AccountPoolQueue(value).Validate(); err != nil {
		return err
	}
	*queue = AccountPoolQueue(value)
	return nil
}

func (queue AccountPoolQueue) Validate() error {
	if queue.MaxWaitingRequests < 0 || queue.MaxWaitingRequests > MaxPoolWaiting {
		return fmt.Errorf("max_waiting_requests must be between 0 and %d", MaxPoolWaiting)
	}
	if queue.TimeoutMS < 100 || queue.TimeoutMS > 60000 {
		return fmt.Errorf("timeout_ms must be between 100 and 60000")
	}
	return nil
}

type AccountPool struct {
	ID        AccountPoolID        `json:"id"`
	Name      string               `json:"name"`
	Provider  SubscriptionProvider `json:"provider"`
	Enabled   bool                 `json:"enabled"`
	Sticky    ChannelStickiness    `json:"sticky"`
	Queue     AccountPoolQueue     `json:"queue"`
	Proxy     *ServiceProxy        `json:"proxy,omitempty"`
	CreatedAt time.Time            `json:"created_at"`
	UpdatedAt time.Time            `json:"updated_at"`
}

func DefaultAccountPool() AccountPool {
	return AccountPool{
		Enabled: true,
		Sticky:  ChannelStickiness{Enabled: true, TTLSeconds: 3600},
		Queue:   AccountPoolQueue{MaxWaitingRequests: DefaultPoolWaiting, TimeoutMS: DefaultPoolWaitMS},
	}
}

func (pool AccountPool) Validate() error {
	if err := pool.ID.Validate(); err != nil {
		return err
	}
	if strings.TrimSpace(pool.Name) != pool.Name || utf8.RuneCountInString(pool.Name) < 1 || utf8.RuneCountInString(pool.Name) > 128 || strings.ContainsAny(pool.Name, "\x00\r\n") {
		return fmt.Errorf("pool name must contain 1 to 128 characters without surrounding whitespace or control characters")
	}
	if !pool.Provider.Valid() {
		return fmt.Errorf("invalid pool provider")
	}
	if err := pool.Sticky.Validate(); err != nil {
		return fmt.Errorf("sticky: %w", err)
	}
	if err := pool.Queue.Validate(); err != nil {
		return fmt.Errorf("queue: %w", err)
	}
	return pool.Proxy.ValidateForPool(pool.ID)
}

type AccountPoolMember struct {
	PoolID                AccountPoolID `json:"pool_id"`
	ServiceID             ServiceID     `json:"service_id"`
	Enabled               bool          `json:"enabled"`
	MaxConcurrentRequests int           `json:"max_concurrent_requests"`
}

// AccountPoolDocument is the complete non-secret configuration exchanged by
// sync. Membership is stored relationally, but travels atomically with its pool.
type AccountPoolDocument struct {
	AccountPool
	Members []AccountPoolMember `json:"members"`
}

func (member AccountPoolMember) Validate() error {
	if err := member.PoolID.Validate(); err != nil {
		return err
	}
	if err := member.ServiceID.Validate(); err != nil {
		return err
	}
	if member.MaxConcurrentRequests < 1 || member.MaxConcurrentRequests > MaxAccountConcurrency {
		return fmt.Errorf("max_concurrent_requests must be between 1 and %d", MaxAccountConcurrency)
	}
	return nil
}

// ValidateMember never upgrades an account's authorization mode. Claude pools
// use the existing direct OAuth path only, including its established empty mode.
func (pool AccountPool) ValidateMember(service Service) error {
	if !service.Kind.IsSubscription() || service.Kind.SubscriptionProvider() != pool.Provider || service.Subscription == nil || service.Subscription.Provider != pool.Provider {
		return fmt.Errorf("pool member must be a subscription of the pool provider")
	}
	if pool.Provider == SubscriptionProviderClaudeCode && service.Subscription.Mode == SubscriptionModeOfficial {
		return fmt.Errorf("Claude pools require direct connection; CLI accounts are unsupported")
	}
	return nil
}

type RoutingScope string

const (
	RoutingScopeGlobal RoutingScope = "global"
	RoutingScopePool   RoutingScope = "pool"
)

type AccessTokenRouting struct {
	RoutingScope RoutingScope  `json:"routing_scope"`
	PoolID       AccountPoolID `json:"pool_id,omitempty"`
}

func (routing AccessTokenRouting) Validate() error {
	switch routing.RoutingScope {
	case RoutingScopeGlobal:
		if routing.PoolID != "" {
			return fmt.Errorf("global routing must not specify pool_id")
		}
	case RoutingScopePool:
		if err := routing.PoolID.Validate(); err != nil {
			return fmt.Errorf("pool routing requires a valid pool_id: %w", err)
		}
	default:
		return fmt.Errorf("routing_scope must be explicitly global or pool")
	}
	return nil
}

// AccountQuotaCooldown stores only the exhausted account windows. Recovery
// probes and in-flight ownership stay in the single process scheduler.
type AccountQuotaCooldown struct {
	ServiceID  ServiceID            `json:"service_id"`
	ObservedAt time.Time            `json:"observed_at"`
	Source     string               `json:"source"`
	Windows    []AccountQuotaWindow `json:"windows"`
}

type AccountQuotaWindow struct {
	ID                 string     `json:"id"`
	LimitWindowSeconds *int64     `json:"limit_window_seconds,omitempty"`
	ResetAt            *time.Time `json:"reset_at,omitempty"`
}

func (cooldown AccountQuotaCooldown) Validate() error {
	if err := cooldown.ServiceID.Validate(); err != nil {
		return err
	}
	if cooldown.ObservedAt.IsZero() || len(cooldown.Windows) == 0 || len(cooldown.Windows) > 32 {
		return fmt.Errorf("quota cooldown requires an observation and 1 to 32 exhausted windows")
	}
	if cooldown.Source != "usage" && cooldown.Source != "upstream" {
		return fmt.Errorf("invalid cooldown source")
	}
	seen := make(map[string]bool)
	for _, window := range cooldown.Windows {
		if window.ID == "" || len(window.ID) > 128 || seen[window.ID] {
			return fmt.Errorf("invalid or duplicate cooldown window")
		}
		seen[window.ID] = true
		if window.LimitWindowSeconds != nil && *window.LimitWindowSeconds <= 0 {
			return fmt.Errorf("invalid cooldown window duration")
		}
		if window.ResetAt != nil && window.ResetAt.IsZero() {
			return fmt.Errorf("invalid cooldown reset time")
		}
	}
	return nil
}

// RetryAt is known only if every blocking window supplies a reset time.
func (cooldown AccountQuotaCooldown) RetryAt() time.Time {
	var latest time.Time
	for _, window := range cooldown.Windows {
		if window.ResetAt == nil {
			return time.Time{}
		}
		if window.ResetAt.After(latest) {
			latest = *window.ResetAt
		}
	}
	return latest
}
