package controlapi

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QuantumNous/astrlink/core/contract"
	"github.com/QuantumNous/astrlink/core/internal/accountauth"
	"github.com/QuantumNous/astrlink/core/internal/subscription"
)

func TestKiroSubscriptionCreatePersistAndDelete(t *testing.T) {
	ctx := context.Background()
	store, err := openControlTestStore(t, ctx, t.TempDir()+"/kiro-test.sqlite")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	manager, err := subscription.NewManager(subscription.StorageAccountStore{Store: store}, accountauth.NewMemoryCredentialStore(), accountauth.OAuthConfig{})
	if err != nil {
		t.Fatal(err)
	}
	handler, err := NewWithDependencies(contract.DefaultVersionResponse("test", "abc1234"), Dependencies{ServiceStore: store, Subscriptions: manager, ControlToken: testControlToken})
	if err != nil {
		t.Fatal(err)
	}
	etag := ""
	call := func(method, path, body string, status int) []byte {
		t.Helper()
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Authorization", "Bearer "+testControlToken)
		if method == "DELETE" {
			r.Header.Set("If-Match", etag)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != status {
			t.Fatalf("%s %s: status=%d body=%s", method, path, w.Code, w.Body.String())
		}
		if value := w.Header().Get("ETag"); value != "" {
			etag = value
		}
		return w.Body.Bytes()
	}
	var capabilities contract.CapabilitiesResponse
	if err := json.Unmarshal(call("GET", "/control/v1/capabilities", "", 200), &capabilities); err != nil {
		t.Fatal(err)
	}
	advertised := false
	for _, plan := range capabilities.PlanTypes {
		if plan.ID == contract.PlanTypeAdapter {
			advertised = plan.UsesLocalConversion && !plan.AvailableInAlpha
		}
	}
	if !advertised {
		t.Fatal("runtime did not advertise provider adaptation")
	}
	var service contract.Service
	if err := json.Unmarshal(call("POST", ServicesPath, `{"name":"Kiro","kind":"kiro_subscription","responses_websocket_enabled":true}`, 201), &service); err != nil {
		t.Fatal(err)
	}
	if service.Subscription == nil || service.Subscription.Provider != contract.SubscriptionProviderKiro || service.ResponsesWebSocket() {
		t.Fatalf("invalid Kiro service: %+v", service)
	}
	account, err := manager.Get(ctx, service.ID)
	if err != nil || account.Provider != contract.SubscriptionProviderKiro {
		t.Fatalf("stored account=%+v err=%v", account, err)
	}
	for _, capability := range service.Capabilities {
		if capability.Mode != contract.CapabilityModeAdapter {
			t.Fatalf("Kiro API advertises native forwarding: %+v", capability)
		}
	}
	path := ServicesPath + "/" + string(service.ID)
	call("POST", path+"/authorization", `{"flow":"browser"}`, 422)
	call("POST", path+"/authorization", `{"flow":"authorization_code"}`, 422)
	call("DELETE", path, "", 204)
	if _, err := store.GetService(ctx, service.ID); err == nil {
		t.Fatal("deleted Kiro service remains persisted")
	}
}
