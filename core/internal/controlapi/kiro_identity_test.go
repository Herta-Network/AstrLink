package controlapi

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/QuantumNous/astrlink/core/contract"
	"github.com/QuantumNous/astrlink/core/internal/accountauth"
	"github.com/QuantumNous/astrlink/core/internal/endpoint"
	"github.com/QuantumNous/astrlink/core/internal/providerapi"
)

func TestKiroIdentitySettingsPersistAndApplyWithoutRestart(t *testing.T) {
	store, handler := newRouteHandler(t)
	registry := accountauth.NewIdentityRegistry(store, store)
	client := http.Header{}
	providerapi.ApplyKiroIdentity(client, providerapi.KiroIdentityAt("0.12.0", true))
	if changed, err := registry.LearnKiro(context.Background(), client); err != nil || !changed {
		t.Fatalf("learning=%t %v", changed, err)
	}
	restarted := accountauth.NewIdentityRegistry(store, store)
	if err := restarted.Hydrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	authorizer := endpoint.NewServiceAuthorizer(nil, identityTestTokens{}).WithRoutingSettings(store).WithIdentities(restarted)
	providerapi.ApplyKiroIdentity(client, providerapi.KiroIdentityAt("0.14.0", true))
	for _, step := range []struct {
		patch, version string
		enforce, learn bool
	}{
		{`{"kiro_identity_version":"0.13.0"}`, "0.13.0", true, true},
		{`{"kiro_identity_enforcement":false}`, "0.14.0", false, true},
		{`{"kiro_identity_enforcement":true,"kiro_identity_auto_learn":false,"kiro_identity_version":""}`, accountauth.DefaultKiroClientVersion, true, false},
		{`{"kiro_identity_auto_learn":true}`, "0.12.0", true, true},
	} {
		response := controlRequest(t, handler, http.MethodPatch, RoutingSettingsPath, "application/merge-patch+json", step.patch, "")
		if response.Code != 200 {
			t.Fatalf("PATCH: %d %s", response.Code, response.Body.String())
		}
		saved, err := store.GetRoutingSettings(context.Background())
		if err != nil || saved.KiroIdentityEnforcement != step.enforce || saved.KiroIdentityAutoLearn != step.learn {
			t.Fatalf("settings=%+v err=%v", saved, err)
		}
		h, err := authorizer.Headers(context.Background(), contract.Endpoint{ID: "service_kiro", Kind: contract.ServiceKindKiroSubscription}, client)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(h.Get("User-Agent"), "KiroIDE-"+step.version) || !strings.Contains(h.Get("X-Amz-User-Agent"), "KiroIDE-"+step.version) {
			t.Fatalf("settings not applied: %v", h)
		}
		if !saved.CodexIdentityEnforcement || !saved.ClaudeIdentityEnforcement || !saved.GrokIdentityEnforcement {
			t.Fatal("Kiro settings changed another provider")
		}
	}
	for _, patch := range []string{`{"kiro_identity_enforcement":null}`, `{"kiro_identity_auto_learn":"true"}`, `{"kiro_identity_version":"not-a-version"}`} {
		if res := controlRequest(t, handler, http.MethodPatch, RoutingSettingsPath, "application/merge-patch+json", patch, ""); res.Code == 200 {
			t.Fatalf("accepted invalid patch: %s", patch)
		}
	}
}
