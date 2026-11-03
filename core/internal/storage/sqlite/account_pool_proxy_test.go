package sqlite

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"testing"
	"time"

	"github.com/QuantumNous/astrlink/core/contract"
	"github.com/QuantumNous/astrlink/core/internal/networkproxy"
	"github.com/QuantumNous/astrlink/core/internal/storage"
)

func TestPoolProxyInheritanceOverrideAndOperationSnapshot(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t, filepath.Join(t.TempDir(), "pool-proxy.db"))
	defer store.Close()
	server := func(label string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, label) }))
	}
	origin, global, poolProxy, accountProxy := server("direct"), server("global"), server("pool"), server("account")
	defer origin.Close()
	defer global.Close()
	defer poolProxy.Close()
	defer accountProxy.Close()
	transport := http.DefaultTransport.(*http.Transport).Clone()
	globalURL, _ := url.Parse(global.URL)
	transport.Proxy = http.ProxyURL(globalURL)
	client := networkproxy.WrapClient(&http.Client{Transport: transport, Timeout: time.Second})
	defer client.CloseIdleConnections()
	account := poolTestAccount(t, store, "service_pool_proxy", contract.SubscriptionProviderKiro, "")
	pool := poolTestCreate(t, store, "pool_proxy", contract.SubscriptionProviderKiro)
	var err error
	pool, err = store.ReplaceAccountPoolMembers(ctx, pool.Pool.ID, []contract.AccountPoolMember{{PoolID: pool.Pool.ID, ServiceID: account.Service.ID, Enabled: true, MaxConcurrentRequests: 2}}, pool.ETag)
	if err != nil {
		t.Fatal(err)
	}
	bind := func() context.Context {
		t.Helper()
		bound, err := networkproxy.Resolver(store, store)(ctx, account.Service.ID)
		if err != nil {
			t.Fatal(err)
		}
		return bound
	}
	fetch := func(bound context.Context, want string) {
		t.Helper()
		request, _ := http.NewRequestWithContext(bound, http.MethodGet, origin.URL, nil)
		response, err := client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		body, err := io.ReadAll(response.Body)
		if err != nil || string(body) != want {
			t.Fatalf("exit=%q want=%q err=%v", body, want, err)
		}
	}
	setPool := func(proxy *contract.ServiceProxy) {
		t.Helper()
		pool.Pool.Proxy = proxy
		pool, err = store.UpdateAccountPool(ctx, pool.Pool, storage.PoolProxyMutation{}, pool.ETag)
		if err != nil {
			t.Fatal(err)
		}
	}
	setAccount := func(proxy *contract.ServiceProxy) {
		t.Helper()
		account.Service.Proxy = proxy
		account, err = store.UpdateService(ctx, account.Service, storage.CredentialMutation{}, account.ETag)
		if err != nil {
			t.Fatal(err)
		}
	}
	fetch(bind(), "global")
	setPool(&contract.ServiceProxy{Mode: "custom", URL: poolProxy.URL})
	snapshot := bind()
	fetch(snapshot, "pool")
	setAccount(&contract.ServiceProxy{Mode: "custom", URL: accountProxy.URL})
	fetch(bind(), "account")
	setAccount(&contract.ServiceProxy{Mode: "direct"})
	fetch(bind(), "direct")
	setAccount(&contract.ServiceProxy{Mode: "inherit"})
	fetch(bind(), "pool")
	setPool(&contract.ServiceProxy{Mode: "direct"})
	fetch(bind(), "direct")
	fetch(snapshot, "pool")
	setPool(&contract.ServiceProxy{Mode: "inherit"})
	fetch(bind(), "global")
	setPool(&contract.ServiceProxy{Mode: "custom", URL: poolProxy.URL})
	poolProxy.Close()
	request, _ := http.NewRequestWithContext(bind(), http.MethodGet, origin.URL, nil)
	if response, err := client.Do(request); err == nil {
		response.Body.Close()
		t.Fatal("failed pool proxy fell back to another exit")
	}
}
