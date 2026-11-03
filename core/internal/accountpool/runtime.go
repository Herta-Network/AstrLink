package accountpool

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/QuantumNous/astrlink/core/contract"
	"github.com/QuantumNous/astrlink/core/internal/storage"
)

type Store interface {
	storage.AccountPoolStore
	storage.AccountQuotaStore
	storage.AccessTokenRoutingStore
	GetService(context.Context, contract.ServiceID) (storage.ServiceRecord, error)
}

type Runtime struct {
	Scheduler *Scheduler
	store     Store
	usage     interface {
		CachedUsage(contract.ServiceID) (contract.SubscriptionUsage, bool)
	}
	quotaMu      sync.Mutex
	usageMu      sync.Mutex
	seenUsage    map[contract.ServiceID]time.Time
	readingUsage map[contract.ServiceID]*usageRead
}

type usageRead struct {
	done chan struct{}
	err  error
}

func NewRuntime(ctx context.Context, store Store) (*Runtime, error) {
	runtime := &Runtime{Scheduler: NewScheduler(), store: store, seenUsage: map[contract.ServiceID]time.Time{}, readingUsage: map[contract.ServiceID]*usageRead{}}
	cooldowns, err := store.ListAccountQuotaCooldowns(ctx)
	if err != nil {
		return nil, fmt.Errorf("load account quota cooldowns: %w", err)
	}
	for _, cooldown := range cooldowns {
		if err := runtime.Scheduler.SetCooldown(cooldown); err != nil {
			return nil, err
		}
	}
	if observed, ok := store.(interface {
		SetAccountPoolObserver(storage.AccountPoolObserver)
	}); ok {
		observed.SetAccountPoolObserver(runtime.Scheduler)
	}
	return runtime, nil
}

func (runtime *Runtime) WithUsageCache(cache interface {
	CachedUsage(contract.ServiceID) (contract.SubscriptionUsage, bool)
}) *Runtime {
	runtime.usage = cache
	return runtime
}

func (runtime *Runtime) observeCachedUsage(ctx context.Context, id contract.ServiceID) error {
	if runtime.usage == nil {
		return nil
	}
	usage, ok := runtime.usage.CachedUsage(id)
	if !ok || usage.FetchedAt.IsZero() {
		return nil
	}
	for {
		runtime.usageMu.Lock()
		if !usage.FetchedAt.After(runtime.seenUsage[id]) {
			runtime.usageMu.Unlock()
			return nil
		}
		if reading := runtime.readingUsage[id]; reading != nil {
			runtime.usageMu.Unlock()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-reading.done:
				if reading.err != nil {
					return reading.err
				}
				continue
			}
		}
		reading := &usageRead{done: make(chan struct{})}
		runtime.readingUsage[id] = reading
		runtime.usageMu.Unlock()
		err := runtime.ObserveUsage(ctx, id, usage)
		runtime.usageMu.Lock()
		reading.err = err
		delete(runtime.readingUsage, id)
		close(reading.done)
		runtime.usageMu.Unlock()
		return err
	}
}

func (runtime *Runtime) ValidatePrincipal(ctx context.Context, original storage.AccessTokenMetadata) error {
	current, err := runtime.store.GetAccessToken(ctx, original.ID)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return ErrChanged
		}
		return ErrUnavailable
	}
	if current.AccessTokenRouting != original.AccessTokenRouting || current.RoutingRevision != original.RoutingRevision {
		return ErrChanged
	}
	return nil
}

// View resolves fresh membership for either a pool key or one globally chosen
// account. Global traffic uses the same slot counter when that account is pooled.
func (runtime *Runtime) View(ctx context.Context, principal storage.AccessTokenMetadata, candidates []contract.ServiceID, admin bool) (View, error) {
	view := View{Candidates: candidates, Admin: admin}
	if len(candidates) == 0 {
		return view, ErrUnavailable
	}
	if !admin {
		if principal.ID == "" || principal.AccessTokenRouting.Validate() != nil {
			return view, ErrNotBound
		}
		if err := runtime.ValidatePrincipal(ctx, principal); err != nil {
			return view, err
		}
	}
	var record storage.AccountPoolRecord
	var err error
	if principal.RoutingScope == contract.RoutingScopePool && !admin {
		record, err = runtime.store.GetAccountPool(ctx, principal.PoolID)
		if err != nil {
			if errors.Is(err, storage.ErrNotFound) {
				return view, ErrNotBound
			}
			return view, ErrUnavailable
		}
	} else {
		view.PreserveOrder = true
		var found bool
		var pool contract.AccountPool
		var member contract.AccountPoolMember
		pool, member, found, err = runtime.store.AccountPoolMembership(ctx, candidates[0])
		record = storage.AccountPoolRecord{Pool: pool, Members: []contract.AccountPoolMember{member}}
		if err != nil {
			return view, ErrUnavailable
		}
		if !found {
			return view, nil
		}
		view.Candidates = candidates[:1]
	}
	view.Pool, view.Members = &record.Pool, record.Members
	for _, id := range view.Candidates {
		if err := runtime.observeCachedUsage(ctx, id); err != nil {
			return view, ErrUnavailable
		}
	}
	return view, nil
}

func (runtime *Runtime) Finish(ctx context.Context, permit *Permit, success bool) error {
	runtime.quotaMu.Lock()
	defer runtime.quotaMu.Unlock()
	if permit == nil {
		return nil
	}
	if recovered := permit.Finish(success); recovered != nil {
		if err := runtime.store.DeleteAccountQuotaCooldown(ctx, permit.ServiceID, *recovered); err != nil {
			_ = runtime.Scheduler.SetCooldown(*recovered)
			return err
		}
	}
	return nil
}

func (runtime *Runtime) ObserveExhaustion(ctx context.Context, id contract.ServiceID, windows []contract.AccountQuotaWindow) (bool, error) {
	runtime.quotaMu.Lock()
	defer runtime.quotaMu.Unlock()
	if _, _, found, err := runtime.store.AccountPoolMembership(ctx, id); err != nil {
		return false, err
	} else if !found {
		return false, nil
	}
	if len(windows) == 0 {
		windows = []contract.AccountQuotaWindow{{ID: "account"}}
	}
	value := contract.AccountQuotaCooldown{ServiceID: id, ObservedAt: time.Now().UTC(), Source: "upstream", Windows: append([]contract.AccountQuotaWindow(nil), windows...)}
	if current, ok := runtime.Scheduler.Cooldown(id); ok {
		seen := map[string]bool{}
		for _, window := range windows {
			seen[window.ID] = true
		}
		for _, old := range current.Windows {
			if !seen[old.ID] {
				value.Windows = append(value.Windows, old)
			}
		}
	}
	return true, runtime.setCooldown(ctx, value)
}

func (runtime *Runtime) setCooldown(ctx context.Context, value contract.AccountQuotaCooldown) error {
	if err := runtime.Scheduler.SetCooldown(value); err != nil {
		return err
	}
	return runtime.store.PutAccountQuotaCooldown(ctx, value)
}

// ObserveUsage never turns an elapsed reset time into available quota by
// itself. Only the provider's newer complete window evidence can clear it.
func (runtime *Runtime) ObserveUsage(ctx context.Context, id contract.ServiceID, usage contract.SubscriptionUsage) (err error) {
	runtime.quotaMu.Lock()
	defer runtime.quotaMu.Unlock()
	current, cooling := runtime.Scheduler.Cooldown(id)
	if !cooling {
		if _, _, found, err := runtime.store.AccountPoolMembership(ctx, id); err != nil {
			return err
		} else if !found {
			return nil
		}
	}
	if usage.FetchedAt.IsZero() {
		return fmt.Errorf("usage observation has no timestamp")
	}
	defer func() {
		if err == nil {
			runtime.usageMu.Lock()
			if usage.FetchedAt.After(runtime.seenUsage[id]) {
				runtime.seenUsage[id] = usage.FetchedAt
			}
			runtime.usageMu.Unlock()
		}
	}()
	if cooling && !usage.FetchedAt.After(current.ObservedAt) {
		return nil
	}
	windows := map[string]*contract.RateLimitWindow{"primary": usage.Primary, "secondary": usage.Secondary}
	value := contract.AccountQuotaCooldown{ServiceID: id, Source: "usage", ObservedAt: usage.FetchedAt}
	for _, name := range []string{"primary", "secondary"} {
		window := windows[name]
		if window == nil || window.UsedPercent < 100 {
			continue
		}
		entry := contract.AccountQuotaWindow{ID: name, LimitWindowSeconds: window.LimitWindowSeconds, ResetAt: window.ResetAt}
		if entry.ResetAt == nil && window.ResetAfterSeconds != nil && *window.ResetAfterSeconds >= 0 {
			at := usage.FetchedAt.Add(time.Duration(*window.ResetAfterSeconds) * time.Second)
			entry.ResetAt = &at
		}
		value.Windows = append(value.Windows, entry)
	}
	flagged := usage.Allowed != nil && !*usage.Allowed || usage.LimitReached != nil && *usage.LimitReached
	if flagged && len(value.Windows) == 0 {
		value.Windows = []contract.AccountQuotaWindow{{ID: "account"}}
	}
	if len(value.Windows) > 0 {
		// A response that omits a previously exhausted window cannot erase it.
		if cooling {
			for _, old := range current.Windows {
				if old.ID != "account" && windows[old.ID] == nil {
					value.Windows = append(value.Windows, old)
				}
			}
		}
		return runtime.setCooldown(ctx, value)
	}
	if !cooling {
		return nil
	}
	for _, blocked := range current.Windows {
		if blocked.ID == "account" {
			known := usage.Primary != nil || usage.Secondary != nil || usage.Allowed != nil && *usage.Allowed || usage.LimitReached != nil && !*usage.LimitReached
			if !known {
				return nil
			}
			continue
		}
		if windows[blocked.ID] == nil {
			return nil
		}
	}
	if err := runtime.store.DeleteAccountQuotaCooldown(ctx, id, current); err != nil {
		return err
	}
	runtime.Scheduler.ClearCooldown(current)
	return nil
}

// UsageObservedAt reports cache freshness without causing a provider query.
func (runtime *Runtime) UsageObservedAt(id contract.ServiceID) *time.Time {
	if runtime.usage == nil {
		return nil
	}
	usage, ok := runtime.usage.CachedUsage(id)
	if !ok || usage.FetchedAt.IsZero() {
		return nil
	}
	at := usage.FetchedAt
	return &at
}
