package accountauth

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/QuantumNous/astrlink/core/contract"
	"github.com/QuantumNous/astrlink/core/internal/providerapi"
)

func kiroIdentityHeader(version string) http.Header {
	h := http.Header{}
	providerapi.ApplyKiroIdentity(h, providerapi.KiroIdentityAt(version, true))
	return h
}

func TestKiroIdentityLearningPersistenceAndFloor(t *testing.T) {
	ctx := context.Background()
	store := &memoryIdentityStore{}
	registry := NewIdentityRegistry(nil, store)
	h := kiroIdentityHeader("0.12.0")
	machine := strings.Repeat("a", 64)
	for _, name := range []string{"User-Agent", "X-Amz-User-Agent"} {
		h.Set(name, h.Get(name)+"-"+machine)
	}
	h.Set("Authorization", "private-token")
	if changed, err := registry.LearnKiro(ctx, h); err != nil || !changed {
		t.Fatalf("learn: %t %v", changed, err)
	}
	document := string(store.documents[contract.SubscriptionProviderKiro])
	if strings.Contains(document, machine) || strings.Contains(document, "private-token") {
		t.Fatalf("persisted request secrets: %s", document)
	}
	for _, version := range []string{"0.10.0", "0.11.107", "99.0.0"} {
		if changed, err := registry.LearnKiro(ctx, kiroIdentityHeader(version)); err != nil || changed {
			t.Fatalf("accepted downgrade/spoof %s", version)
		}
	}
	restarted := NewIdentityRegistry(nil, store)
	if err := restarted.Hydrate(ctx); err != nil {
		t.Fatal(err)
	}
	settings := contract.DefaultRoutingSettings()
	if got := restarted.KiroIdentity(settings).Version; got != "0.12.0" {
		t.Fatalf("hydrated=%s", got)
	}
	settings.KiroIdentityVersion = "0.13.0"
	identity := restarted.KiroIdentity(settings)
	if !strings.Contains(identity.UserAgent, "KiroIDE-0.13.0") || !strings.Contains(identity.Headers["X-Amz-User-Agent"], "KiroIDE-0.13.0") {
		t.Fatalf("unpaired floor: %+v", identity)
	}
	settings.KiroIdentityVersion = ""
	settings.KiroIdentityAutoLearn = false
	if got := restarted.KiroIdentity(settings).Version; got != DefaultKiroClientVersion {
		t.Fatalf("learning off selected %s", got)
	}
	if restarted.ClientIdentities().Kiro.LearnedVersion != "0.12.0" {
		t.Fatal("disabling learning erased stored version")
	}
	bad := kiroIdentityHeader("0.14.0")
	bad.Set("X-Amz-User-Agent", "aws-sdk-js/1.0.34 KiroIDE-0.13.0")
	if changed, _ := registry.LearnKiro(ctx, bad); changed {
		t.Fatal("mismatched identity learned")
	}
	bad = kiroIdentityHeader("0.14.0")
	bad.Set("User-Agent", bad.Get("User-Agent")+" AstrLink")
	if changed, _ := registry.LearnKiro(ctx, bad); changed {
		t.Fatal("gateway-branded identity learned")
	}
}

func TestKiroForwardIdentityToggleAndAPIPairing(t *testing.T) {
	client := kiroIdentityHeader("0.14.0")
	for _, enforce := range []bool{false, true} {
		h := http.Header{}
		ApplyKiroForwardHeaders(h, AccountTokens{AccessToken: "token"}, client, kiroIdentityAt("0.13.0"), enforce)
		want := "0.14.0"
		if enforce {
			want = "0.13.0"
		}
		identity, ok := providerapi.RecognizeKiroIdentity(h)
		if !ok || identity.Version != want || h.Get("Authorization") != "Bearer token" {
			t.Fatalf("enforce=%t header=%v", enforce, h)
		}
	}
	h := http.Header{}
	ApplyKiroForwardHeaders(h, AccountTokens{}, http.Header{"User-Agent": {"AstrLink"}}, kiroIdentityAt("0.13.0"), false)
	if strings.Contains(h.Get("User-Agent"), "AstrLink") || !strings.Contains(h.Get("User-Agent"), "KiroIDE-0.13.0") {
		t.Fatal("unknown client did not fall back")
	}
	ApplyKiroAPIHeaders(h, AccountTokens{AccessToken: "token"}, kiroIdentityAt("0.13.0"))
	if !strings.Contains(h.Get("User-Agent"), "api/codewhispererruntime#") || !strings.Contains(h.Get("X-Amz-User-Agent"), "KiroIDE-0.13.0") {
		t.Fatal("REST identity does not match selected Kiro version")
	}
}
