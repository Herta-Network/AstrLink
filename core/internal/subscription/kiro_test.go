package subscription

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/astrlink/core/contract"
	"github.com/QuantumNous/astrlink/core/internal/accountauth"
)

func TestKiroModelsPaginationAndUsage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.UserAgent(), "KiroIDE-0.13.0") || !strings.Contains(r.Header.Get("X-Amz-User-Agent"), "KiroIDE-0.13.0") {
			t.Errorf("catalog/usage identity not resolved: %v", r.Header)
		}
		if r.Header.Get("Authorization") != "Bearer account-token" || r.URL.Query().Get("origin") != "AI_EDITOR" || r.URL.Query().Get("profileArn") != "test-profile" {
			t.Errorf("missing identity/query: %v %s", r.Header, r.URL)
		}
		for key, values := range r.Header {
			if strings.Contains(strings.ToLower(key+strings.Join(values, "")), "astrlink") {
				t.Errorf("gateway header escaped: %s", key)
			}
		}
		if r.URL.Path == "/ListAvailableModels" {
			if r.URL.Query().Get("nextToken") == "" {
				io.WriteString(w, `{"models":[{"modelId":"claude-sonnet-4.6"}],"nextToken":"next+page"}`)
			} else {
				if r.URL.Query().Get("nextToken") != "next+page" {
					t.Error("cursor not preserved")
				}
				io.WriteString(w, `{"models":[{"modelId":"claude-sonnet-4.6"},{"modelId":"claude-haiku-4.5"}]}`)
			}
		} else if r.URL.Path == "/getUsageLimits" {
			io.WriteString(w, `{"subscriptionInfo":{"subscriptionType":"PRO"},"usageBreakdownList":[{"resourceType":"CREDIT","currentUsage":20,"usageLimit":100,"freeTrialInfo":{"freeTrialStatus":"ACTIVE","currentUsage":5,"usageLimit":50},"bonuses":[{"status":"EXPIRED","currentUsage":9,"usageLimit":999}]}]}`)
		} else {
			t.Errorf("unexpected path %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	manager, err := NewManager(NewMemoryAccountStore(), accountauth.NewMemoryCredentialStore(), accountauth.OAuthConfig{}, accountauth.OAuthConfig{Identities: accountauth.NewIdentityRegistry(kiroIdentitySettings{}, nil), Provider: contract.SubscriptionProviderKiro, APIBaseURL: server.URL, HTTPClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	tokens := accountauth.AccountTokens{AccessToken: "account-token", ProjectID: "test-profile"}
	models, err := manager.KiroModels(context.Background(), tokens)
	if err != nil || strings.Join(models, ",") != "claude-haiku-4.5,claude-sonnet-4.6" {
		t.Fatalf("models=%v err=%v", models, err)
	}
	usage, err := manager.kiroUsage(context.Background(), tokens)
	if err != nil || usage.PlanType != "PRO" || usage.Primary == nil || usage.Primary.UsedPercent != 100.0*25/150 {
		t.Fatalf("usage=%+v err=%v", usage, err)
	}
}

func TestKiroUsageRejectsUnknownOrNegativeAllowance(t *testing.T) {
	for _, body := range []string{`{}`, `{"usageBreakdownList":[]}`, `{"usageBreakdownList":[{"resourceType":"CREDIT","currentUsage":-1,"usageLimit":20}]}`} {
		if _, err := DecodeKiroUsage([]byte(body), time.Now()); err == nil {
			t.Fatalf("accepted invalid usage: %s", body)
		}
	}
}

type kiroIdentitySettings struct{}

func (kiroIdentitySettings) GetRoutingSettings(context.Context) (contract.RoutingSettings, error) {
	s := contract.DefaultRoutingSettings()
	s.KiroIdentityVersion = "0.13.0"
	return s, nil
}
