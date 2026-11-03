package accountpool

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/QuantumNous/astrlink/core/contract"
)

func poolView(limits ...int) View {
	pool := contract.DefaultAccountPool()
	pool.ID, pool.Name, pool.Provider = "pool_a", "A", contract.SubscriptionProviderKiro
	view := View{Pool: &pool}
	for i, limit := range limits {
		id := contract.ServiceID(fmt.Sprintf("service_%d", i))
		view.Members = append(view.Members, contract.AccountPoolMember{PoolID: pool.ID, ServiceID: id, Enabled: true, MaxConcurrentRequests: limit})
		view.Candidates = append(view.Candidates, id)
	}
	return view
}

func requestView(view View) Request {
	return Request{Load: func(context.Context) (View, error) { return view, nil }, BodyBytes: 1024}
}

func mustAcquire(t *testing.T, s *Scheduler, view View) *Permit {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	p, err := s.Acquire(ctx, requestView(view))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func waitForQueued(t *testing.T, s *Scheduler, count int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	for {
		changed := s.Changes()
		_, n := s.Counts("", "pool_a")
		if n == count {
			return
		}
		select {
		case <-changed:
		case <-ctx.Done():
			t.Fatalf("waiting=%d, want %d", n, count)
		}
	}
}

func TestNewSessionsDistributeAndSuccessfulSessionsStay(t *testing.T) {
	s := NewScheduler()
	base := poolView(2, 2, 2)
	for i := 0; i < 6; i++ {
		view := base
		view.StickyKey = fmt.Sprintf("new-session-%d", i)
		p := mustAcquire(t, s, view)
		if p.ServiceID != base.Candidates[i%3] {
			t.Fatalf("serial tie did not rotate: %s", p.ServiceID)
		}
		p.Finish(true)
	}
	view := base
	view.StickyKey = "new-session-0"
	for i := 0; i < 3; i++ {
		p := mustAcquire(t, s, view)
		if p.ServiceID != base.Candidates[0] {
			t.Fatal("successful session migrated")
		}
		p.Finish(true)
	}
}

func TestConcurrentAdmissionReservesBeforeReturning(t *testing.T) {
	s := NewScheduler()
	view := poolView(1, 1, 1)
	var wg sync.WaitGroup
	results := make(chan *Permit, 3)
	errors := make(chan error, 3)
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			p, err := s.Acquire(ctx, requestView(view))
			if err != nil {
				errors <- err
				return
			}
			results <- p
		}()
	}
	wg.Wait()
	close(results)
	close(errors)
	for err := range errors {
		t.Fatal(err)
	}
	used := map[contract.ServiceID]bool{}
	for p := range results {
		if used[p.ServiceID] {
			t.Fatal("same final slot admitted twice")
		}
		used[p.ServiceID] = true
		p.Finish(false)
		p.Finish(false)
		if n, _ := s.Counts(p.ServiceID, "pool_a"); n != 0 {
			t.Fatalf("double release corrupted count: %d", n)
		}
	}
	if len(used) != 3 {
		t.Fatal("requests were not distributed")
	}
}

func TestLeastOccupiedRatioUsesAccountCapacity(t *testing.T) {
	s := NewScheduler()
	view := poolView(1, 4)
	p1 := mustAcquire(t, s, view)
	defer p1.Finish(false)
	p2 := mustAcquire(t, s, view)
	defer p2.Finish(false)
	p3 := mustAcquire(t, s, view)
	defer p3.Finish(false)
	if p2.ServiceID != view.Candidates[1] || p3.ServiceID != view.Candidates[1] {
		t.Fatal("capacity ratio was ignored")
	}
}

func TestStickyBusyWaitsButQuotaExhaustionSwitches(t *testing.T) {
	s := NewScheduler()
	view := poolView(1, 1)
	view.StickyKey = "same-session"
	first := mustAcquire(t, s, view)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan *Permit, 1)
	failures := make(chan error, 1)
	go func() {
		p, err := s.Acquire(ctx, requestView(view))
		if err != nil {
			failures <- err
			return
		}
		result <- p
	}()
	waitForQueued(t, s, 1)
	select {
	case <-result:
		t.Fatal("concurrency congestion migrated sticky session")
	case err := <-failures:
		t.Fatal(err)
	default:
	}
	if err := s.SetCooldown(contract.AccountQuotaCooldown{ServiceID: first.ServiceID, ObservedAt: time.Now().UTC(), Source: "usage", Windows: []contract.AccountQuotaWindow{{ID: "weekly"}}}); err != nil {
		t.Fatal(err)
	}
	select {
	case p := <-result:
		if p.ServiceID == first.ServiceID {
			t.Fatal("exhausted account kept the request")
		}
		p.Finish(true)
		// A late successful completion from the old account cannot undo migration.
		first.Finish(true)
		if next := mustAcquire(t, s, view); next.ServiceID != p.ServiceID {
			next.Finish(false)
			t.Fatal("late completion restored old binding")
		} else {
			next.Finish(true)
		}
	case err := <-failures:
		t.Fatal(err)
	case <-time.After(time.Second):
		t.Fatal("quota update did not wake the request")
	}
}

func TestQueueCancellationLimitsAndAccountFairness(t *testing.T) {
	s := NewScheduler()
	view := poolView(1, 1)
	view.Pool.Queue.MaxWaitingRequests = 1
	view.Preferred, view.Strict = view.Candidates[0], true
	held := mustAcquire(t, s, view)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		p, err := s.Acquire(ctx, requestView(view))
		if p != nil {
			p.Finish(false)
		}
		done <- err
	}()
	waitForQueued(t, s, 1)
	if _, err := s.Acquire(context.Background(), requestView(view)); !errors.Is(err, ErrQueueFull) {
		t.Fatalf("queue limit: %v", err)
	}
	other := view
	other.Preferred = view.Candidates[1]
	p := mustAcquire(t, s, other)
	p.Finish(true)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancel did not remove waiter")
	}
	waitForQueued(t, s, 0)
	held.Finish(false)
	if n, _ := s.Counts(view.Candidates[0], "pool_a"); n != 0 {
		t.Fatal("cancel leaked a slot")
	}
}

func TestQueueBodyBudgetAndExpiry(t *testing.T) {
	s := NewScheduler()
	view := poolView(1)
	view.Pool.Queue.TimeoutMS = 100
	held := mustAcquire(t, s, view)
	defer held.Finish(false)
	tooLarge := requestView(view)
	tooLarge.BodyBytes = MaxWaitingBytes + 1
	if _, err := s.Acquire(context.Background(), tooLarge); !errors.Is(err, ErrQueueFull) {
		t.Fatalf("byte limit: %v", err)
	}
	if _, err := s.Acquire(context.Background(), requestView(view)); !errors.Is(err, ErrBusy) {
		t.Fatalf("queue deadline: %v", err)
	}
	if _, waiting := s.Counts("", "pool_a"); waiting != 0 {
		t.Fatal("expired request remains queued")
	}
}

func TestQuotaRecoveryRequiresAllWindowsAndOneProbe(t *testing.T) {
	s := NewScheduler()
	view := poolView(2)
	now := time.Now().UTC()
	short, week := now.Add(-time.Second), now.Add(time.Hour)
	q := contract.AccountQuotaCooldown{ServiceID: view.Candidates[0], ObservedAt: now, Source: "usage", Windows: []contract.AccountQuotaWindow{{ID: "short", ResetAt: &short}, {ID: "week", ResetAt: &week}}}
	if err := s.SetCooldown(q); err != nil {
		t.Fatal(err)
	}
	if s.AllowProbe(q.ServiceID, false) {
		t.Fatal("short window prematurely cleared weekly quota")
	}
	if _, err := s.Acquire(context.Background(), requestView(view)); !errors.Is(err, ErrQuota) {
		t.Fatal(err)
	}
	q.Windows[1].ResetAt = &short
	if err := s.SetCooldown(q); err != nil {
		t.Fatal(err)
	}
	if !s.AllowProbe(q.ServiceID, false) {
		t.Fatal("expired windows could not be probed")
	}
	p := mustAcquire(t, s, view)
	if !p.Probe {
		t.Fatal("missing half-open marker")
	}
	if _, err := s.Acquire(context.Background(), requestView(view)); !errors.Is(err, ErrQuota) {
		t.Fatalf("multiple probes were admitted: %v", err)
	}
	newer := q
	newer.ObservedAt = q.ObservedAt.Add(time.Second)
	if err := s.SetCooldown(newer); err != nil {
		t.Fatal(err)
	}
	if p.Finish(true) != nil || s.ClearCooldown(q) {
		t.Fatal("old probe cleared newer exhaustion")
	}
}

func TestExplicitBindingReleaseDropsSuccessfulAndInFlightPreferences(t *testing.T) {
	scheduler := NewScheduler()
	view := poolView(2, 2)
	view.StickyKey = "client-conversation"
	view.SessionID = "session_one"
	first := mustAcquire(t, scheduler, view)
	first.Finish(true)
	scheduler.ForgetSession(view.SessionID)
	second := mustAcquire(t, scheduler, view)
	if second.ServiceID == first.ServiceID {
		t.Fatal("released successful binding stayed cached")
	}
	scheduler.ForgetSession(view.SessionID)
	second.Finish(true)
	third := mustAcquire(t, scheduler, view)
	defer third.Finish(false)
	if third.ServiceID == second.ServiceID {
		t.Fatal("in-flight completion recreated released binding")
	}
}
