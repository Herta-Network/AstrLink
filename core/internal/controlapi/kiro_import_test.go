package controlapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QuantumNous/astrlink/core/contract"
	"github.com/QuantumNous/astrlink/core/internal/accountauth"
	"github.com/QuantumNous/astrlink/core/internal/storage"
	"github.com/QuantumNous/astrlink/core/internal/subscription"
)

func TestKiroFileImportUsesServiceCredentialTransaction(t *testing.T) {
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var in map[string]string
		if r.URL.Path != "/refreshToken" || json.NewDecoder(r.Body).Decode(&in) != nil || len(in) != 1 {
			t.Error("unexpected upstream request")
			w.WriteHeader(400)
			return
		}
		if in["refreshToken"] == "invalid" {
			w.WriteHeader(401)
			w.Write([]byte(`{"error":"invalid_grant"}`))
			return
		}
		w.Write([]byte(`{"accessToken":"mock-access","refreshToken":"mock-rotated","expiresIn":3600,"profileArn":"arn:aws:codewhisperer:us-east-1:123456789012:profile/mock"}`))
	}))
	defer upstream.Close()
	ctx := context.Background()
	store, err := openControlTestStore(t, ctx, t.TempDir()+"/import.sqlite")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	credentials := subscription.NewStorageAccountCredentialStore(store)
	manager, err := subscription.NewManager(subscription.StorageAccountStore{Store: store}, credentials, accountauth.OAuthConfig{}, accountauth.OAuthConfig{Provider: contract.SubscriptionProviderKiro, KiroSocialBaseURL: upstream.URL, APIBaseURL: upstream.URL, HTTPClient: upstream.Client()})
	if err != nil {
		t.Fatal(err)
	}
	handler, err := NewWithDependencies(contract.DefaultVersionResponse("test", "test"), Dependencies{ControlToken: testControlToken, ObserverToken: testObserverToken, Subscriptions: manager, ServiceStore: store})
	if err != nil {
		t.Fatal(err)
	}
	post := func(path, body, token string, status int) []byte {
		t.Helper()
		r := httptest.NewRequest("POST", path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != status {
			t.Fatalf("got %d, want %d (%s)", w.Code, status, w.Body.String())
		}
		for _, v := range []string{"mock-access", "mock-refresh", "mock-rotated"} {
			if strings.Contains(w.Body.String(), v) {
				t.Fatal("response disclosed credential")
			}
		}
		return w.Body.Bytes()
	}
	create := `{"name":"Kiro test","kind":"kiro_subscription","kiro_import":{"provider":"Google","refreshToken":"mock-refresh"}}`
	post(ServicesPath, create, testObserverToken, 403)
	post(ServicesPath, strings.Replace(create, "mock-refresh", "invalid", 1), testControlToken, 422)
	if _, err := store.GetService(ctx, kiroImportServiceID("invalid")); !errors.Is(err, storage.ErrNotFound) {
		t.Fatal("failed import created a service")
	}
	var service contract.Service
	if err := json.Unmarshal(post(ServicesPath, create, testControlToken, 201), &service); err != nil {
		t.Fatal(err)
	}
	if service.Subscription.Status != contract.SubscriptionStatusConnected || service.Subscription.TokenExpiresAt == nil {
		t.Fatal("not connected")
	}
	post(ServicesPath, create, testControlToken, 409)
	if calls != 2 {
		t.Fatalf("unexpected number of refreshes: %d", calls)
	}
	path := ServicesPath + "/" + string(service.ID) + "/kiro-import"
	post(path, `{"provider":"Google","refreshToken":"invalid"}`, testObserverToken, 403)
	post(path, `{"provider":"Google","refreshToken":"invalid"}`, testControlToken, 422)
	check := func() {
		t.Helper()
		tokens, err := manager.AccessToken(ctx, service.ID)
		if err != nil || tokens.RefreshToken != "mock-rotated" || tokens.AccessToken != "mock-access" || tokens.KiroAuthMethod != "social" {
			t.Fatalf("saved credential unavailable: %v", err)
		}
	}
	check()
	post(path, `{"provider":"GitHub","refreshToken":"mock-refresh"}`, testControlToken, 200)
	check()
}
