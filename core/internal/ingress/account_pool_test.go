package ingress

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/astrlink/core/contract"
	"github.com/QuantumNous/astrlink/core/internal/accesstoken"
	"github.com/QuantumNous/astrlink/core/internal/accountpool"
	"github.com/QuantumNous/astrlink/core/internal/endpoint"
	"github.com/QuantumNous/astrlink/core/internal/storage"
	"github.com/QuantumNous/astrlink/core/internal/storage/sqlite"
	"github.com/QuantumNous/astrlink/core/internal/transport"
)

func TestPoolKeyCannotEscapeThroughPinModelDiscoveryOrConversion(t *testing.T) {
	ctx := context.Background()
	store, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "pools.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	var calls atomic.Int32
	upstream := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		if r.Header.Get("Authorization") != "Bearer service_a" {
			t.Errorf("wrong account selected: %s", r.Header.Get("Authorization"))
		}
		body := `{"id":"msg_a","type":"message","role":"assistant","model":"model-a","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":2,"output_tokens":1}}`
		if r.URL.Path == "/v1/models" {
			body = `{"object":"list","data":[{"id":"model-a","object":"model"},{"id":"model-b","object":"model"}]}`
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
	})

	for _, suffix := range []string{"a", "b"} {
		service := contract.Service{ID: contract.ServiceID("service_" + suffix), Name: suffix, Kind: contract.ServiceKindClaudeSubscription, Enabled: true, Models: []string{"model-" + suffix}, Capabilities: contract.SubscriptionProviderClaudeCode.Capabilities(), Subscription: &contract.SubscriptionConnection{Provider: contract.SubscriptionProviderClaudeCode, Mode: contract.SubscriptionModeLegacy, Status: contract.SubscriptionStatusConnected, CredentialRef: "local://subscription/service_" + suffix}}
		if _, err := store.CreateService(ctx, service, storage.CredentialMutation{}); err != nil {
			t.Fatal(err)
		}
		pool := contract.DefaultAccountPool()
		pool.ID, pool.Name, pool.Provider = contract.AccountPoolID("pool_"+suffix), suffix, contract.SubscriptionProviderClaudeCode
		record, err := store.CreateAccountPool(ctx, pool, storage.PoolProxyMutation{})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.ReplaceAccountPoolMembers(ctx, pool.ID, []contract.AccountPoolMember{{PoolID: pool.ID, ServiceID: service.ID, Enabled: true, MaxConcurrentRequests: 2}}, record.ETag); err != nil {
			t.Fatal(err)
		}
	}
	tokens, _ := accesstoken.NewManager(store)
	key, err := tokens.Create(ctx, "pool A", contract.AccessTokenRouting{RoutingScope: contract.RoutingScopePool, PoolID: "pool_a"})
	if err != nil {
		t.Fatal(err)
	}
	resolver, _ := endpoint.NewStoreResolver(store)
	runtime, err := accountpool.NewRuntime(ctx, store)
	if err != nil {
		t.Fatal(err)
	}
	handler, err := NewNetworkProduction(Dependencies{AccountPools: store, PoolRuntime: runtime, Resolver: resolver,
		Authorizer: authorizerFunc(func(_ context.Context, endpoint contract.Endpoint) (http.Header, error) {
			return http.Header{"Authorization": {"Bearer " + string(endpoint.ID)}}, nil
		}),
		Forwarder: transport.New(upstream), ProxyCredentials: store, RequestRecords: store,
		AccessTokenAuthenticator: AccessTokenAuthenticatorFunc(tokens.AuthenticatePrincipal),
	})
	if err != nil {
		t.Fatal(err)
	}
	call := func(method, path, body, pin string, want int) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+key.Value)
		r.Header.Set("Content-Type", "application/json")
		if pin != "" {
			r.Header.Set(ServiceHeader, pin)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != want {
			t.Fatalf("%s %s status=%d want=%d: %s", method, path, w.Code, want, w.Body.String())
		}
		return w
	}
	body := `{"model":"model-a","max_tokens":32,"messages":[{"role":"user","content":"hello"}]}`
	call("POST", "/v1/messages", body, "", 200)
	before := calls.Load()
	call("POST", "/v1/messages", body, "service_b", 403)
	call("POST", "/v1/messages", strings.ReplaceAll(body, "model-a", "model-b"), "", 400)
	call("POST", "/v1/chat/completions", body, "", 400)
	if calls.Load() != before {
		t.Fatal("out-of-scope request reached upstream")
	}
	catalog := call("GET", "/v1/models", "", "", 200)
	var models struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(catalog.Body.Bytes(), &models); err != nil {
		t.Fatal(err)
	}
	for _, model := range models.Data {
		if model.ID == "model-b" {
			t.Fatal("model discovery leaked models outside the pool")
		}
	}
	pool, _ := store.GetAccountPool(ctx, "pool_a")
	pool.Pool.Enabled = false
	if _, err := store.UpdateAccountPool(ctx, pool.Pool, storage.PoolProxyMutation{}, pool.ETag); err != nil {
		t.Fatal(err)
	}
	call("POST", "/v1/messages", body, "", 503)
}

// poolGatewayFixture uses the real SQLite, authentication, resolver, privacy,
// recording and ingress pipeline. The synthetic round tripper cannot dial a
// provider even though subscription endpoints keep their production URLs.
func poolGatewayFixture(t *testing.T, count, limit int, upstream http.RoundTripper) (*Handler, *sqlite.Store, *accountpool.Runtime, *accesstoken.Manager, string) {
	t.Helper()
	return poolGatewayProviderFixture(t, count, limit, upstream, contract.SubscriptionProviderClaudeCode)
}

func poolGatewayProviderFixture(t *testing.T, count, limit int, upstream http.RoundTripper, provider contract.SubscriptionProvider) (*Handler, *sqlite.Store, *accountpool.Runtime, *accesstoken.Manager, string) {
	t.Helper()
	ctx := context.Background()
	store, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "pool-gateway.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	pool := contract.DefaultAccountPool()
	pool.ID, pool.Name, pool.Provider = "pool_gateway", "Gateway", provider
	record, err := store.CreateAccountPool(ctx, pool, storage.PoolProxyMutation{})
	if err != nil {
		t.Fatal(err)
	}
	members := []contract.AccountPoolMember{}
	for i := 0; i < count; i++ {
		id := contract.ServiceID(fmt.Sprintf("service_pool_%03d", i))
		service := contract.Service{ID: id, Name: string(id), Kind: provider.ServiceKind(), Enabled: true, Models: []string{"model-test"}, Capabilities: provider.Capabilities(), Subscription: &contract.SubscriptionConnection{Provider: provider, Mode: contract.SubscriptionModeLegacy, Status: contract.SubscriptionStatusConnected, CredentialRef: "local://subscription/" + string(id)}}
		if _, err := store.CreateService(ctx, service, storage.CredentialMutation{}); err != nil {
			t.Fatal(err)
		}
		members = append(members, contract.AccountPoolMember{PoolID: pool.ID, ServiceID: id, Enabled: true, MaxConcurrentRequests: limit})
	}
	if _, err := store.ReplaceAccountPoolMembers(ctx, pool.ID, members, record.ETag); err != nil {
		t.Fatal(err)
	}
	tokens, err := accesstoken.NewManager(store)
	if err != nil {
		t.Fatal(err)
	}
	key, err := tokens.Create(ctx, "Pool gateway", contract.AccessTokenRouting{RoutingScope: contract.RoutingScopePool, PoolID: pool.ID})
	if err != nil {
		t.Fatal(err)
	}
	resolver, err := endpoint.NewStoreResolver(store)
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := accountpool.NewRuntime(ctx, store)
	if err != nil {
		t.Fatal(err)
	}
	handler, err := NewNetworkProduction(Dependencies{AccountPools: store, PoolRuntime: runtime, Resolver: resolver,
		Authorizer: authorizerFunc(func(_ context.Context, e contract.Endpoint) (http.Header, error) {
			return http.Header{"Authorization": {"Bearer " + string(e.ID)}}, nil
		}),
		Forwarder: transport.New(upstream), ProxyCredentials: store, RequestRecords: store,
		AccessTokenAuthenticator: AccessTokenAuthenticatorFunc(tokens.AuthenticatePrincipal),
	})
	if err != nil {
		t.Fatal(err)
	}
	return handler, store, runtime, tokens, key.Value
}

func poolGatewayRequest(ctx context.Context, token, session string, stream bool) *http.Request {
	user, _ := json.Marshal(map[string]string{"device_id": "test", "account_uuid": "", "session_id": session})
	raw, _ := json.Marshal(map[string]any{"model": "model-test", "max_tokens": 32, "stream": stream, "metadata": map[string]string{"user_id": string(user)}, "messages": []map[string]string{{"role": "user", "content": "hello " + session}}})
	r := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(string(raw))).WithContext(ctx)
	r.Header.Set("Authorization", "Bearer "+token)
	r.Header.Set("Content-Type", "application/json")
	return r
}

type poolTestStream struct {
	ctx             context.Context
	prefix, tail    *strings.Reader
	release, closed chan struct{}
	once            sync.Once
}

func (stream *poolTestStream) Read(p []byte) (int, error) {
	if stream.prefix.Len() > 0 {
		return stream.prefix.Read(p)
	}
	select {
	case <-stream.ctx.Done():
		return 0, stream.ctx.Err()
	case <-stream.closed:
		return 0, io.ErrClosedPipe
	case <-stream.release:
	}
	return stream.tail.Read(p)
}
func (stream *poolTestStream) Close() error {
	stream.once.Do(func() { close(stream.closed) })
	return nil
}
func newPoolTestStream(ctx context.Context) *poolTestStream {
	return &poolTestStream{ctx: ctx, release: make(chan struct{}), closed: make(chan struct{}),
		prefix: strings.NewReader("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_pool\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"model-test\",\"content\":[],\"usage\":{\"input_tokens\":2,\"output_tokens\":0}}}\n\nevent: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\nevent: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"ok\"}}\n\n"),
		tail:   strings.NewReader("event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\nevent: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"),
	}
}

func TestPoolGatewayStreamLifetimeStickyWaitingAndCancellation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	type arrival struct {
		id     contract.ServiceID
		stream *poolTestStream
	}
	arrived := make(chan arrival, 4)
	handler, store, runtime, _, token := poolGatewayFixture(t, 2, 1, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		stream := newPoolTestStream(r.Context())
		id := contract.ServiceID(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		arrived <- arrival{id, stream}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: stream}, nil
	}))
	var serving sync.WaitGroup
	t.Cleanup(func() { cancel(); serving.Wait() })
	launch := func(request *http.Request) <-chan *httptest.ResponseRecorder {
		done := make(chan *httptest.ResponseRecorder, 1)
		go func() { w := httptest.NewRecorder(); handler.ServeHTTP(w, request); done <- w }()
		return done
	}
	take := func(done <-chan *httptest.ResponseRecorder) arrival {
		t.Helper()
		select {
		case value := <-arrived:
			return value
		case w := <-done:
			t.Fatalf("request finished before upstream: %d %s", w.Code, w.Body.String())
		case <-ctx.Done():
			t.Fatal("upstream admission timed out")
		}
		return arrival{}
	}
	finish := func(done <-chan *httptest.ResponseRecorder) {
		t.Helper()
		select {
		case w := <-done:
			if w.Code != 200 {
				t.Fatalf("stream status: %d %s", w.Code, w.Body.String())
			}
		case <-ctx.Done():
			t.Fatal("stream did not finish")
		}
	}
	session := "00000000-0000-4000-8000-000000000001"
	firstDone := launch(poolGatewayRequest(ctx, token, session, true))
	first := take(firstDone)
	if active, _ := runtime.Scheduler.Counts(first.id, "pool_gateway"); active != 1 {
		t.Fatalf("stream released before body: %d", active)
	}
	secondCtx, stopSecond := context.WithCancel(ctx)
	defer stopSecond()
	secondDone := launch(poolGatewayRequest(secondCtx, token, session, true))
	for {
		changed := runtime.Scheduler.Changes()
		_, waiting := runtime.Scheduler.Counts(first.id, "pool_gateway")
		if waiting == 1 {
			break
		}
		select {
		case <-changed:
		case wrong := <-arrived:
			t.Fatalf("sticky request escaped to %s", wrong.id)
		case w := <-secondDone:
			t.Fatalf("waiter exited: %d %s", w.Code, w.Body.String())
		case <-ctx.Done():
			t.Fatal("request did not queue")
		}
	}
	thirdDone := launch(poolGatewayRequest(ctx, token, "00000000-0000-4000-8000-000000000002", true))
	third := take(thirdDone)
	if third.id == first.id {
		t.Fatal("parallel requests exceeded the member cap")
	}
	close(third.stream.release)
	finish(thirdDone)
	select {
	case wrong := <-arrived:
		t.Fatalf("sticky waiter moved to idle account %s", wrong.id)
	default:
	}
	close(first.stream.release)
	finish(firstDone)
	second := take(secondDone)
	if second.id != first.id {
		t.Fatal("sticky request changed accounts")
	}
	stopSecond()
	finish(secondDone)
	for _, id := range []contract.ServiceID{first.id, third.id} {
		if active, waiting := runtime.Scheduler.Counts(id, "pool_gateway"); active != 0 || waiting != 0 {
			t.Fatalf("leaked slots: %s active=%d waiting=%d", id, active, waiting)
		}
	}
	records, err := store.ListRequestRecords(ctx, storage.RequestRecordListOptions{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(records.Items) != 3 {
		t.Fatalf("records: %d", len(records.Items))
	}
	for _, record := range records.Items {
		if record.RoutingDecision == nil || record.RoutingDecision.Pool == nil || record.RoutingDecision.Pool.ID != "pool_gateway" {
			t.Fatalf("missing pool attribution: %+v", record.RoutingDecision)
		}
	}
}

func TestPoolGatewayQuotaFailoverPreservesBothWindowsAndRecovery(t *testing.T) {
	ctx := context.Background()
	short, week := time.Now().Add(time.Hour).Truncate(time.Second), time.Now().Add(48*time.Hour).Truncate(time.Second)
	var callsMu sync.Mutex
	var calls []string
	var exhaustFirst atomic.Bool
	exhaustFirst.Store(true)
	handler, store, runtime, _, token := poolGatewayFixture(t, 2, 1, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		id := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		callsMu.Lock()
		calls = append(calls, id)
		callsMu.Unlock()
		if id == "service_pool_000" && exhaustFirst.Load() {
			return &http.Response{StatusCode: 429, Header: http.Header{
				"Content-Type":                          {"application/json"},
				"Anthropic-Ratelimit-Unified-5h-Status": {"rejected"}, "Anthropic-Ratelimit-Unified-5h-Reset": {strconv.FormatInt(short.Unix(), 10)},
				"Anthropic-Ratelimit-Unified-7d-Status": {"rejected"}, "Anthropic-Ratelimit-Unified-7d-Reset": {strconv.FormatInt(week.Unix(), 10)},
			}, Body: io.NopCloser(strings.NewReader(`{"type":"error","error":{"type":"rate_limit_error","message":"usage limit reached"}}`))}, nil
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"id":"msg_test","type":"message","role":"assistant","model":"model-test","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":2,"output_tokens":1}}`))}, nil
	}))
	run := func(want int) *httptest.ResponseRecorder {
		t.Helper()
		r := poolGatewayRequest(ctx, token, "00000000-0000-4000-8000-000000000009", false)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != want {
			t.Fatalf("status=%d want=%d: %s", w.Code, want, w.Body.String())
		}
		return w
	}
	run(200)
	callsMu.Lock()
	firstCalls := append([]string(nil), calls...)
	callsMu.Unlock()
	if len(firstCalls) != 2 || firstCalls[0] != "service_pool_000" || firstCalls[1] != "service_pool_001" {
		t.Fatalf("quota failover calls: %v", firstCalls)
	}
	current, ok := runtime.Scheduler.Cooldown("service_pool_000")
	if !ok || len(current.Windows) != 2 || !current.RetryAt().Equal(week) {
		t.Fatalf("lost upstream window evidence: %+v", current)
	}
	service, err := store.GetService(ctx, "service_pool_000")
	if err != nil {
		t.Fatal(err)
	}
	if service.Service.Subscription.Status != contract.SubscriptionStatusConnected || service.Service.Subscription.Risk != nil {
		t.Fatal("quota changed login or generic risk state")
	}
	usage := contract.SubscriptionUsage{FetchedAt: time.Now().Add(time.Second), Primary: &contract.RateLimitWindow{UsedPercent: 1}}
	if err := runtime.ObserveUsage(ctx, "service_pool_000", usage); err != nil {
		t.Fatal(err)
	}
	if _, cooling := runtime.Scheduler.Cooldown("service_pool_000"); !cooling {
		t.Fatal("missing weekly quota restored the account")
	}
	_, err = runtime.ObserveExhaustion(ctx, "service_pool_001", []contract.AccountQuotaWindow{{ID: "secondary", ResetAt: &week}})
	if err != nil {
		t.Fatal(err)
	}
	rejected := run(429)
	if !strings.Contains(rejected.Body.String(), "pool_quota_exhausted") {
		t.Fatal(rejected.Body.String())
	}
	retry, err := strconv.Atoi(rejected.Header().Get("Retry-After"))
	if err != nil || retry < 172790 {
		t.Fatalf("retry reset ignored the weekly window: %q", rejected.Header().Get("Retry-After"))
	}
	exhaustFirst.Store(false)
	usage.FetchedAt = time.Now().Add(2 * time.Second)
	usage.Secondary = &contract.RateLimitWindow{UsedPercent: 1}
	if err := runtime.ObserveUsage(ctx, "service_pool_000", usage); err != nil {
		t.Fatal(err)
	}
	run(200)
	callsMu.Lock()
	defer callsMu.Unlock()
	if calls[len(calls)-1] != "service_pool_000" {
		t.Fatalf("fresh complete recovery did not restore account: %v", calls)
	}
}

func TestPoolGatewayGlobalKeyAndControlTestShareCapacity(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	arrivals := make(chan *poolTestStream, 3)
	handler, store, runtime, tokens, key := poolGatewayFixture(t, 1, 1, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		stream := newPoolTestStream(r.Context())
		arrivals <- stream
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: stream}, nil
	}))
	var workers sync.WaitGroup
	t.Cleanup(func() { cancel(); workers.Wait() })
	serve := func(r *http.Request, admin bool) <-chan *httptest.ResponseRecorder {
		done := make(chan *httptest.ResponseRecorder, 1)
		workers.Add(1)
		go func() {
			defer workers.Done()
			w := httptest.NewRecorder()
			if admin {
				record, err := store.GetService(ctx, "service_pool_000")
				if err != nil {
					t.Error(err)
					done <- w
					return
				}
				err = handler.ServeServiceTest(w, r, record.Service, contract.ServiceTestRequest{Protocol: contract.ProtocolAnthropicMessages, Model: "model-test", Prompt: "hello", Stream: true}, "https://provider.invalid", ServiceTestObserver{})
				if err != nil && ctx.Err() == nil {
					t.Error(err)
				}
			} else {
				handler.ServeHTTP(w, r)
			}
			done <- w
		}()
		return done
	}
	firstDone := serve(poolGatewayRequest(ctx, key, "first", true), false)
	var first *poolTestStream
	select {
	case first = <-arrivals:
	case w := <-firstDone:
		t.Fatalf("pool call ended: %d %s", w.Code, w.Body.String())
	case <-ctx.Done():
		t.Fatal("pool call did not start")
	}
	global, err := tokens.Create(ctx, "global", contract.AccessTokenRouting{RoutingScope: contract.RoutingScopeGlobal})
	if err != nil {
		t.Fatal(err)
	}
	request := poolGatewayRequest(ctx, global.Value, "global", true)
	request.Header.Set(ServiceHeader, "service_pool_000")
	globalDone := serve(request, false)
	adminDone := serve(poolGatewayRequest(ctx, "", "admin", true), true)
	waitCount := func(want int) {
		t.Helper()
		for {
			changed := runtime.Scheduler.Changes()
			_, waiting := runtime.Scheduler.Counts("service_pool_000", "pool_gateway")
			if waiting == want {
				return
			}
			select {
			case <-changed:
			case <-arrivals:
				t.Fatal("another path exceeded shared account capacity")
			case w := <-globalDone:
				t.Fatalf("global ended early: %d %s", w.Code, w.Body.String())
			case w := <-adminDone:
				t.Fatalf("control test ended early: %d %s", w.Code, w.Body.String())
			case <-ctx.Done():
				t.Fatalf("queue never reached %d", want)
			}
		}
	}
	waitCount(2)
	if _, err := tokens.UpdateRouting(ctx, global.Token.ID, contract.AccessTokenRouting{RoutingScope: contract.RoutingScopePool, PoolID: "pool_gateway"}, global.Token.ETag()); err != nil {
		t.Fatal(err)
	}
	select {
	case w := <-globalDone:
		if w.Code != 403 || !strings.Contains(w.Body.String(), "pool_scope_changed") {
			t.Fatalf("rebound waiter: %d %s", w.Code, w.Body.String())
		}
	case <-arrivals:
		t.Fatal("rebound waiter reached upstream")
	case <-ctx.Done():
		t.Fatal("rebound key did not wake")
	}
	close(first.release)
	select {
	case w := <-firstDone:
		if w.Code != 200 {
			t.Fatal(w.Body.String())
		}
	case <-ctx.Done():
		t.Fatal("first request did not finish")
	}
	var admin *poolTestStream
	select {
	case admin = <-arrivals:
	case w := <-adminDone:
		t.Fatalf("admin test ended early: %d %s", w.Code, w.Body.String())
	case <-ctx.Done():
		t.Fatal("admin test did not resume")
	}
	close(admin.release)
	select {
	case w := <-adminDone:
		if w.Code != 200 {
			t.Fatal(w.Body.String())
		}
	case <-ctx.Done():
		t.Fatal("admin test did not finish")
	}
	if active, waiting := runtime.Scheduler.Counts("service_pool_000", "pool_gateway"); active != 0 || waiting != 0 {
		t.Fatalf("capacity leak: %d %d", active, waiting)
	}
}

func TestPoolGatewayContinuationCannotCrossPoolsOrUseAnotherAccount(t *testing.T) {
	var calls atomic.Int32
	handler, store, runtime, tokens, key := poolGatewayProviderFixture(t, 1, 1, roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return nil, errors.New("unexpected upstream attempt")
	}), contract.SubscriptionProviderOpenAICodex)
	ctx := context.Background()
	principal, err := tokens.AuthenticatePrincipal(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	binding := storage.ResponseAffinity{PoolID: "pool_other", ServiceID: "service_pool_000", UpstreamModel: "model-test", UpstreamProtocol: contract.ProtocolOpenAIResponses, PlanType: contract.PlanTypeNative}
	call := func(responseID string, want int) {
		t.Helper()
		body := `{"model":"model-test","input":"continue","previous_response_id":"` + responseID + `"}`
		r := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+key)
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != want {
			t.Fatalf("continuation=%d want=%d: %s", w.Code, want, w.Body.String())
		}
	}
	if err := store.PutResponseAffinity(ctx, string(principal.ID), "resp_crosspool", binding); err != nil {
		t.Fatal(err)
	}
	call("resp_crosspool", 403)
	binding.PoolID = "pool_gateway"
	binding.ServiceID = "service_missing"
	if err := store.PutResponseAffinity(ctx, string(principal.ID), "resp_missing", binding); err != nil {
		t.Fatal(err)
	}
	call("resp_missing", 409)
	binding.ServiceID = "service_pool_000"
	if err := store.PutResponseAffinity(ctx, string(principal.ID), "resp_cooled", binding); err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.ObserveExhaustion(ctx, binding.ServiceID, nil); err != nil {
		t.Fatal(err)
	}
	call("resp_cooled", 409)
	if calls.Load() != 0 {
		t.Fatalf("continuation replayed outside affinity: %d", calls.Load())
	}
}

type concurrentFirstPoolRecords struct {
	*sqlite.Store
	lookups atomic.Int32
	entered chan struct{}
	release chan struct{}
}

func (store *concurrentFirstPoolRecords) FindSessionLink(ctx context.Context, kind contract.SessionCursorKind, values []string, scope storage.SessionCursorScope) (storage.SessionLinkMatch, bool, error) {
	if kind == contract.SessionCursorExplicit && store.lookups.Add(1) <= 2 {
		store.entered <- struct{}{}
		select {
		case <-store.release:
			return storage.SessionLinkMatch{}, false, nil
		case <-ctx.Done():
			return storage.SessionLinkMatch{}, false, ctx.Err()
		}
	}
	return store.Store.FindSessionLink(ctx, kind, values, scope)
}

func TestPoolGatewayConcurrentFirstRequestsShareProvisionalBinding(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	type arrival struct {
		id     string
		stream *poolTestStream
	}
	arrivals := make(chan arrival, 2)
	handler, store, runtime, _, key := poolGatewayFixture(t, 2, 1, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		stream := newPoolTestStream(r.Context())
		arrivals <- arrival{r.Header.Get("Authorization"), stream}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: stream}, nil
	}))
	records := &concurrentFirstPoolRecords{Store: store, entered: make(chan struct{}, 2), release: make(chan struct{})}
	handler.requestRecords = records
	var workers sync.WaitGroup
	t.Cleanup(func() { cancel(); workers.Wait() })
	done := make(chan *httptest.ResponseRecorder, 2)
	for range 2 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, poolGatewayRequest(ctx, key, "00000000-0000-4000-8000-000000000008", true))
			done <- w
		}()
	}
	for range 2 {
		select {
		case <-records.entered:
		case w := <-done:
			t.Fatalf("request ended before resolving session: %d %s", w.Code, w.Body.String())
		case <-ctx.Done():
			t.Fatal("parallel session resolution never began")
		}
	}
	close(records.release)
	var first arrival
	select {
	case first = <-arrivals:
	case w := <-done:
		t.Fatalf("request ended before admission: %d", w.Code)
	case <-ctx.Done():
		t.Fatal("no first admission")
	}
	for {
		changed := runtime.Scheduler.Changes()
		_, waiting := runtime.Scheduler.Counts("service_pool_000", "pool_gateway")
		if waiting == 1 {
			break
		}
		select {
		case <-changed:
		case wrong := <-arrivals:
			t.Fatalf("same new conversation chose conflicting accounts: %s and %s", first.id, wrong.id)
		case w := <-done:
			t.Fatalf("waiter ended: %d %s", w.Code, w.Body.String())
		case <-ctx.Done():
			t.Fatal("second request did not queue")
		}
	}
	close(first.stream.release)
	var second arrival
	select {
	case second = <-arrivals:
	case <-ctx.Done():
		t.Fatal("second request did not resume")
	}
	if second.id != first.id {
		t.Fatalf("provisional binding changed account: %s != %s", second.id, first.id)
	}
	close(second.stream.release)
	for range 2 {
		select {
		case w := <-done:
			if w.Code != 200 {
				t.Fatalf("stream failed: %d %s", w.Code, w.Body.String())
			}
		case <-ctx.Done():
			t.Fatal("requests did not drain")
		}
	}
}

func TestPoolGatewayProxyFailureDoesNotDialDirectAndUsesPoolError(t *testing.T) {
	var proxyCalls atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxyCalls.Add(1)
		w.WriteHeader(http.StatusProxyAuthRequired)
	}))
	defer proxy.Close()
	base := http.DefaultTransport.(*http.Transport).Clone()
	base.Proxy = nil
	proxyAddress := strings.TrimPrefix(proxy.URL, "http://")
	base.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		if address != proxyAddress {
			return nil, fmt.Errorf("unexpected direct connection to %s", address)
		}
		return (&net.Dialer{}).DialContext(ctx, network, address)
	}
	defer base.CloseIdleConnections()
	handler, store, _, _, key := poolGatewayFixture(t, 1, 1, base)
	ctx := context.Background()
	record, err := store.GetAccountPool(ctx, "pool_gateway")
	if err != nil {
		t.Fatal(err)
	}
	record.Pool.Proxy = &contract.ServiceProxy{Mode: "custom", URL: proxy.URL}
	if _, err := store.UpdateAccountPool(ctx, record.Pool, storage.PoolProxyMutation{}, record.ETag); err != nil {
		t.Fatal(err)
	}
	service, err := store.GetService(ctx, "service_pool_000")
	if err != nil {
		t.Fatal(err)
	}
	policy := contract.DefaultFailurePolicy()
	policy.MaxRetries = 0
	policy.NetworkError = contract.FailureStop
	service.Service.FailurePolicy = &policy
	if _, err := store.UpdateService(ctx, service.Service, storage.CredentialMutation{}, service.ETag); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, poolGatewayRequest(ctx, key, "proxy-error", false))
	if w.Code != 502 || !strings.Contains(w.Body.String(), "pool_proxy_unavailable") {
		t.Fatalf("proxy error=%d %s", w.Code, w.Body.String())
	}
	if proxyCalls.Load() != 1 {
		t.Fatalf("unexpected proxy attempts: %d", proxyCalls.Load())
	}
	if strings.Contains(w.Body.String(), proxyAddress) {
		t.Fatal("public error exposed the proxy address")
	}
}
