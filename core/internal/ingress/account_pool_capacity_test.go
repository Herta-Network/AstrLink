package ingress

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/QuantumNous/astrlink/core/contract"
)

// Opt in to a concurrent-stream smoke test. This exercises the real gateway
// and SQLite with synthetic provider I/O, not provider capacity or a long soak.
func TestPoolGatewayConcurrentStreamCapacity(t *testing.T) {
	raw := os.Getenv("ASTRLINK_POOL_TEST_CONCURRENCY")
	if raw == "" {
		t.Skip("set ASTRLINK_POOL_TEST_CONCURRENCY to run the local capacity smoke test")
	}
	concurrency, err := strconv.Atoi(raw)
	if err != nil || concurrency < 1 || concurrency > 2000 {
		t.Fatal("concurrency must be between 1 and 2000")
	}
	members := min(concurrency, 50)
	limit := (concurrency + members - 1) / members
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	type admission struct {
		at     time.Time
		stream *poolTestStream
	}
	admitted := make(chan admission, concurrency)
	handler, _, scheduler, _, token := poolGatewayFixture(t, members, limit, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		stream := newPoolTestStream(r.Context())
		admitted <- admission{time.Now(), stream}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: stream}, nil
	}))
	done := make(chan *httptest.ResponseRecorder, concurrency)
	var work sync.WaitGroup
	finished := make(chan struct{})
	t.Cleanup(func() {
		cancel()
		select {
		case <-finished:
		case <-time.After(10 * time.Second):
			t.Error("gateway requests did not drain after cancellation")
		}
	})
	start := time.Now()
	for i := 0; i < concurrency; i++ {
		work.Add(1)
		go func(i int) {
			defer work.Done()
			r := poolGatewayRequest(ctx, token, fmt.Sprintf("00000000-0000-4000-8000-%012d", i), true)
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			done <- w
		}(i)
	}
	go func() { work.Wait(); close(finished) }()
	latencies := make([]time.Duration, 0, concurrency)
	streams := make([]*poolTestStream, 0, concurrency)
	for len(streams) < concurrency {
		select {
		case entry := <-admitted:
			streams = append(streams, entry.stream)
			latencies = append(latencies, entry.at.Sub(start))
		case w := <-done:
			t.Fatalf("request ended before target execution concurrency: status=%d body=%s admitted=%d/%d", w.Code, w.Body.String(), len(streams), concurrency)
		case <-ctx.Done():
			t.Fatalf("admitted %d/%d streams before deadline", len(streams), concurrency)
		}
	}
	active := 0
	distribution := make([]int, 0, members)
	for i := 0; i < members; i++ {
		count, waiting := scheduler.Scheduler.Counts(contract.ServiceID(fmt.Sprintf("service_pool_%03d", i)), "pool_gateway")
		if count > limit || waiting != 0 {
			t.Fatalf("capacity invariant: count=%d limit=%d waiting=%d", count, limit, waiting)
		}
		active += count
		distribution = append(distribution, count)
	}
	if active != concurrency {
		t.Fatalf("executing=%d want=%d", active, concurrency)
	}
	var peak runtime.MemStats
	runtime.ReadMemStats(&peak)
	goroutines := runtime.NumGoroutine()
	for _, stream := range streams {
		close(stream.release)
	}
	for range concurrency {
		select {
		case w := <-done:
			if w.Code != 200 {
				t.Fatalf("completion status: %d %s", w.Code, w.Body.String())
			}
		case <-ctx.Done():
			t.Fatal("streams did not finish")
		}
	}
	for i := 0; i < members; i++ {
		count, waiting := scheduler.Scheduler.Counts(contract.ServiceID(fmt.Sprintf("service_pool_%03d", i)), "pool_gateway")
		if count != 0 || waiting != 0 {
			t.Fatalf("leaked capacity: active=%d waiting=%d", count, waiting)
		}
	}
	slices.Sort(latencies)
	p := func(percent int) time.Duration { return latencies[(len(latencies)-1)*percent/100] }
	t.Logf("synthetic full-pipeline smoke: members=%d per-account-limit=%d executing=%d distribution=%v; admission from burst start p50=%s p95=%s p99=%s; heap-in-use=%d MiB allocated=%d MiB goroutines=%d; total=%s", members, limit, active, distribution, p(50), p(95), p(99), peak.HeapInuse>>20, peak.Alloc>>20, goroutines, time.Since(start))
}
