package controlapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/QuantumNous/astrlink/core/contract"
	"github.com/QuantumNous/astrlink/core/internal/accesstoken"
	"github.com/QuantumNous/astrlink/core/internal/storage"
)

func TestPoolControlAndAccessTokenScope(t *testing.T) {
	ctx := context.Background()
	store, err := openControlTestStore(t, ctx, filepath.Join(t.TempDir(), "pools.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	tokens, err := accesstoken.NewManager(store)
	if err != nil {
		t.Fatal(err)
	}
	const observer = "observer-token-for-pool-test"
	handler, err := NewWithDependencies(contract.DefaultVersionResponse("test", "test"), Dependencies{ServiceStore: store, AccessTokenManager: tokens, ControlToken: testControlToken, ObserverToken: observer})
	if err != nil {
		t.Fatal(err)
	}
	call := func(method, path, body, etag, token string, want int) *httptest.ResponseRecorder {
		t.Helper()
		request := httptest.NewRequest(method, path, strings.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		if method == http.MethodPatch {
			request.Header.Set("Content-Type", "application/merge-patch+json")
		}
		request.Header.Set("Authorization", "Bearer "+token)
		if etag != "" {
			request.Header.Set("If-Match", etag)
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != want {
			t.Fatalf("%s %s = %d, want %d: %s", method, path, response.Code, want, response.Body.String())
		}
		return response
	}
	created := call(http.MethodPost, AccountPoolsPath, `{"name":"Kiro A","provider":"kiro"}`, "", testControlToken, 201)
	var pool accountPoolResponse
	if err := json.Unmarshal(created.Body.Bytes(), &pool); err != nil {
		t.Fatal(err)
	}
	if pool.ID == "" || pool.Queue.TimeoutMS != 5000 || pool.ETag != created.Header().Get("ETag") {
		t.Fatalf("pool defaults: %+v", pool)
	}
	path := AccountPoolsPath + "/" + string(pool.ID)
	call(http.MethodGet, path, "", "", observer, 200)
	call(http.MethodPatch, path, `{"enabled":false}`, pool.ETag, observer, 403)
	call(http.MethodPatch, path, `{"enabled":false}`, "", testControlToken, 428)
	call(http.MethodPatch, path, `{"provider":"claude_code"}`, pool.ETag, testControlToken, 400)
	service := contract.Service{ID: "service_pool_test", Name: "Account", Kind: contract.ServiceKindKiroSubscription, Enabled: true, Models: []string{"model"}, Capabilities: contract.SubscriptionProviderKiro.Capabilities(), Subscription: &contract.SubscriptionConnection{Provider: contract.SubscriptionProviderKiro, Status: contract.SubscriptionStatusDisconnected}}
	if _, err := store.CreateService(ctx, service, storage.CredentialMutation{}); err != nil {
		t.Fatal(err)
	}
	updated := call(http.MethodPut, path+"/members", `{"members":[{"service_id":"service_pool_test","enabled":true,"max_concurrent_requests":2}]}`, pool.ETag, testControlToken, 200)
	if err := json.Unmarshal(updated.Body.Bytes(), &pool); err != nil {
		t.Fatal(err)
	}
	if len(pool.Members) != 1 || pool.Members[0].PoolID != pool.ID {
		t.Fatalf("members: %+v", pool.Members)
	}
	serviceRecord, err := store.GetService(ctx, service.ID)
	if err != nil {
		t.Fatal(err)
	}
	rejectedDelete := call(http.MethodDelete, ServicesPath+"/"+string(service.ID), "", serviceRecord.ETag, testControlToken, 409)
	if !strings.Contains(rejectedDelete.Body.String(), "pool_membership_conflict") {
		t.Fatal(rejectedDelete.Body.String())
	}
	call(http.MethodPost, AccessTokensPath, `{"name":"missing scope"}`, "", testControlToken, 422)
	call(http.MethodPost, AccessTokensPath, `{"name":"missing pool","routing_scope":"pool"}`, "", testControlToken, 422)
	call(http.MethodPost, AccessTokensPath, `{"name":"unknown pool","routing_scope":"pool","pool_id":"pool_missing"}`, "", testControlToken, 422)
	for _, fields := range []string{`"routing_scope":null`, `"routing_scope":"global","pool_id":null`, `"routing_scope":"global","pool_id":""`, `"routing_scope":"pool","pool_id":null`} {
		call(http.MethodPost, AccessTokensPath, `{"name":"invalid",`+fields+`}`, "", testControlToken, 422)
	}
	key := call(http.MethodPost, AccessTokensPath, `{"name":"pool key","routing_scope":"pool","pool_id":"`+string(pool.ID)+`"}`, "", testControlToken, 201)
	var createdKey accessTokenSecretResponse
	if err := json.Unmarshal(key.Body.Bytes(), &createdKey); err != nil {
		t.Fatal(err)
	}
	if createdKey.Token.RoutingScope != contract.RoutingScopePool || createdKey.Token.PoolID != pool.ID {
		t.Fatalf("wrong key scope: %+v", createdKey.Token)
	}
	identity, err := tokens.Authenticate(ctx, createdKey.AccessToken)
	if err != nil {
		t.Fatal(err)
	}
	principal, err := tokens.Get(ctx, identity)
	if err != nil || principal.PoolID != pool.ID || principal.RoutingRevision != 1 {
		t.Fatalf("authentication lost scope: %+v %v", principal, err)
	}
	keyPath := AccessTokensPath + "/" + string(createdKey.Token.ID)
	call(http.MethodGet, keyPath, "", "", observer, 200)
	call(http.MethodDelete, path, "", pool.ETag, testControlToken, 409)
	call(http.MethodPatch, keyPath, `{"routing_scope":"global"}`, createdKey.Token.ETag, testControlToken, 200)
	call(http.MethodPatch, keyPath, `{"routing_scope":"pool","pool_id":"`+string(pool.ID)+`"}`, createdKey.Token.ETag, testControlToken, 412)
	identity, err = tokens.Authenticate(ctx, createdKey.AccessToken)
	if err != nil {
		t.Fatal(err)
	}
	principal, err = tokens.Get(ctx, identity)
	if err != nil || principal.RoutingScope != contract.RoutingScopeGlobal || principal.PoolID != "" || principal.RoutingRevision != 2 {
		t.Fatalf("scope update failed: %+v %v", principal, err)
	}
	call(http.MethodDelete, path, "", pool.ETag, testControlToken, 204)
}
