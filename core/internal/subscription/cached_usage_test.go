package subscription_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/astrlink/core/contract"
	"github.com/QuantumNous/astrlink/core/internal/accountauth"
)

// TestCachedUsageKeepsTheLastSnapshot checks the read routing orders packages
// by: it never reaches the provider, outlives the cache TTL and every expiry,
// and only signing out drops it.
func TestCachedUsageKeepsTheLastSnapshot(t *testing.T) {
	t.Parallel()
	var usageCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api/codex/usage":
			usageCalls.Add(1)
			_ = json.NewEncoder(writer).Encode(map[string]any{
				"plan_type": "plus",
				"rate_limit": map[string]any{"primary_window": map[string]any{
					"used_percent": 25, "limit_window_seconds": 18000, "reset_after_seconds": 3600,
				}},
			})
		case "/api/codex/rate-limit-reset-credits/consume":
			_ = json.NewEncoder(writer).Encode(map[string]any{"code": "reset", "windows_reset": 1})
		default:
			t.Errorf("unexpected upstream request %s %s", request.Method, request.URL.Path)
			http.NotFound(writer, request)
		}
	}))
	t.Cleanup(upstream.Close)
	fixture := newRiskTestFixture(t, accountauth.OAuthConfig{APIBaseURL: upstream.URL, HTTPClient: upstream.Client()})
	ctx, manager, id := context.Background(), fixture.manager, fixture.id

	if usage, ok := manager.CachedUsage(id); ok {
		t.Fatalf("CachedUsage() before any read = %#v", usage)
	}
	first, err := manager.Usage(ctx, id)
	if err != nil || first.PlanType != "plus" {
		t.Fatalf("Usage() = %#v, %v", first, err)
	}
	if _, err := manager.Usage(ctx, id); err != nil || usageCalls.Load() != 1 {
		t.Fatalf("second Usage() err=%v calls=%d, want the cached snapshot", err, usageCalls.Load())
	}

	for _, step := range []struct {
		name   string
		expire func() error
	}{
		{"report risk", func() error {
			return manager.ReportRisk(ctx, id, coolingObservation(contract.RiskCodeRateLimit5h, fixture.clock.Now().Add(time.Hour)))
		}},
		{"clear risk", func() error { _, err := manager.ClearRisk(ctx, id); return err }},
		{"forget usage", func() error { manager.ForgetUsage(id); return nil }},
		{"consume reset", func() error { _, err := manager.ConsumeReset(ctx, id); return err }},
	} {
		calls := usageCalls.Load()
		if err := step.expire(); err != nil {
			t.Fatalf("%s: %v", step.name, err)
		}
		if cached, ok := manager.CachedUsage(id); !ok || cached.PlanType != "plus" {
			t.Fatalf("%s: CachedUsage() = %#v, %v; want the plan kept", step.name, cached, ok)
		}
		if _, err := manager.Usage(ctx, id); err != nil || usageCalls.Load() != calls+1 {
			t.Fatalf("%s: Usage() err=%v calls=%d, want one new read", step.name, err, usageCalls.Load())
		}
	}

	calls := usageCalls.Load()
	fixture.clock.Advance(time.Hour)
	if cached, ok := manager.CachedUsage(id); !ok || cached.PlanType != "plus" || usageCalls.Load() != calls {
		t.Fatalf("CachedUsage() after the TTL = %#v, %v, calls=%d", cached, ok, usageCalls.Load())
	}
	if _, err := manager.Logout(ctx, id); err != nil {
		t.Fatal(err)
	}
	if usage, ok := manager.CachedUsage(id); ok {
		t.Fatalf("CachedUsage() after sign-out = %#v", usage)
	}
}

func TestUsageCoalescesConcurrentRefreshAndCancelsWaiter(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/codex/usage" {
			t.Errorf("unexpected path %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		calls.Add(1)
		entered <- struct{}{}
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		io.WriteString(w, `{"rate_limit":{"primary_window":{"used_percent":10,"limit_window_seconds":18000}}}`)
	}))
	defer upstream.Close()
	fixture := newRiskTestFixture(t, accountauth.OAuthConfig{APIBaseURL: upstream.URL, HTTPClient: upstream.Client()})
	done := make(chan error, 2)
	go func() { _, err := fixture.manager.Usage(ctx, fixture.id); done <- err }()
	select {
	case <-entered:
	case err := <-done:
		t.Fatalf("leader returned early: %v", err)
	case <-ctx.Done():
		t.Fatal("leader never queried usage")
	}
	waiting, stop := context.WithCancel(ctx)
	stop()
	_, err := fixture.manager.Usage(waiting, fixture.id)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled waiter: %v", err)
	}
	go func() { _, err := fixture.manager.Usage(ctx, fixture.id); done <- err }()
	close(release)
	for range 2 {
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-ctx.Done():
			t.Fatal("usage reads did not finish")
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("overlapping reads made %d queries", calls.Load())
	}
}
