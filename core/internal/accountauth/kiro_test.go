package accountauth

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/astrlink/core/contract"
)

func TestKiroDeviceGrantAndCredentialRotation(t *testing.T) {
	var polls atomic.Int32
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for key, values := range r.Header {
			if strings.Contains(strings.ToLower(key+strings.Join(values, "")), "astrlink") {
				t.Errorf("gateway identity in %s", key)
			}
		}
		if !strings.Contains(r.UserAgent(), "KiroIDE-0.13.0") || !strings.Contains(r.Header.Get("X-Amz-User-Agent"), "KiroIDE-0.13.0") {
			t.Errorf("authorization identity did not use configured version: %v", r.Header)
		}
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/client/register":
			if payload["clientName"] != "Kiro" || payload["clientType"] != "public" {
				t.Errorf("registration=%v", payload)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"clientId": "registered-client", "clientSecret": "registration-secret", "clientSecretExpiresAt": now.Add(24 * time.Hour).Unix()})
		case "/device_authorization":
			if payload["clientId"] != "registered-client" || payload["clientSecret"] != "registration-secret" {
				t.Error("registration lost")
			}
			io.WriteString(w, `{"deviceCode":"device-secret","userCode":"KIRO-TEST","verificationUriComplete":"https://view.awsapps.com/start?user_code=KIRO-TEST","expiresIn":600,"interval":1}`)
		case "/token":
			if payload["clientId"] != "registered-client" || payload["clientSecret"] != "registration-secret" {
				t.Error("token exchange omitted dynamic registration")
			}
			if payload["grantType"] == standardDeviceGrantType {
				if payload["deviceCode"] != "device-secret" {
					t.Error("wrong device code")
				}
				switch polls.Add(1) {
				case 1:
					w.WriteHeader(400)
					io.WriteString(w, `{"error":"authorization_pending"}`)
				case 2:
					w.WriteHeader(400)
					io.WriteString(w, `{"error":"slow_down"}`)
				default:
					io.WriteString(w, `{"accessToken":"access-one","refreshToken":"refresh-one","expiresIn":3600,"profileArn":"profile-one"}`)
				}
			} else {
				if payload["grantType"] != "refresh_token" || payload["refreshToken"] != "refresh-one" {
					t.Errorf("refresh=%v", payload)
				}
				io.WriteString(w, `{"accessToken":"access-two","refreshToken":"refresh-two","expiresIn":7200}`)
			}
		default:
			t.Errorf("unexpected request: %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	config := OAuthConfig{Identities: NewIdentityRegistry(fixedSettings{settings: contract.RoutingSettings{KiroIdentityVersion: "0.13.0"}}, nil), Provider: contract.SubscriptionProviderKiro, Issuer: server.URL, APIBaseURL: server.URL, HTTPClient: server.Client(), Now: func() time.Time { return now }, DevicePollMinInterval: time.Millisecond, DevicePollMaxInterval: time.Millisecond}
	store := NewMemoryCredentialStore()
	completed := make(chan AccountTokens, 1)
	manager := NewSessionManager(config, store, func(ctx context.Context, session contract.AuthorizationSession, tokens AccountTokens) error {
		if err := store.Put(ctx, session.ServiceID, tokens); err != nil {
			return err
		}
		completed <- tokens
		return nil
	})
	defer manager.CancelAllForService("service_kiro")
	if _, err := manager.Begin(context.Background(), "service_kiro", contract.AuthorizationFlowBrowser, nil); err == nil {
		t.Fatal("browser flow accepted")
	}
	session, err := manager.Begin(context.Background(), "service_kiro", contract.AuthorizationFlowDeviceCode, nil)
	if err != nil {
		t.Fatal(err)
	}
	public, _ := json.Marshal(session)
	for _, secret := range []string{"registration-secret", "device-secret", "registered-client"} {
		if strings.Contains(string(public), secret) {
			t.Fatalf("public session leaked %s", secret)
		}
	}
	var tokens AccountTokens
	select {
	case tokens = <-completed:
	case <-time.After(3 * time.Second):
		t.Fatal("Kiro device login did not complete")
	}
	if tokens.OAuthClientID != "registered-client" || tokens.OAuthClientSecret != "registration-secret" || tokens.ProjectID != "profile-one" {
		t.Fatal("credential material was not retained")
	}
	raw, err := tokens.MarshalSecret()
	if err != nil {
		t.Fatal(err)
	}
	reloaded, err := UnmarshalAccountTokens(raw)
	if err != nil {
		t.Fatal(err)
	}
	rotated, err := NewTokenClient(config).RefreshAccount(context.Background(), reloaded)
	if err != nil {
		t.Fatal(err)
	}
	if rotated.RefreshToken != "refresh-two" || rotated.AccessToken != "access-two" || rotated.OAuthClientSecret != tokens.OAuthClientSecret || rotated.ProjectID != "profile-one" {
		t.Fatal("refresh lost account registration or profile")
	}
	rotated.OAuthClientExpiresAt = now.Add(-time.Second)
	if _, err := NewTokenClient(config).RefreshAccount(context.Background(), rotated); !errors.Is(err, ErrInvalidGrant) {
		t.Fatalf("expired registration: %v", err)
	}
}

func TestKiroTokenErrorsDoNotLeakProviderBody(t *testing.T) {
	current := AccountTokens{RefreshToken: "refresh-secret"}
	for _, status := range []int{400, 500} {
		_, err := decodeKiroTokens([]byte(`{"error":"invalid_grant","message":"refresh-secret registration-secret"}`), status, current, time.Now())
		if err == nil || strings.Contains(err.Error(), "secret") {
			t.Fatalf("unsafe error: %v", err)
		}
	}
	for _, body := range []string{`{}`, `{"accessToken":"a","expiresIn":-1}`, `{"accessToken":"a","expiresIn":9999999999999999}`} {
		if _, err := decodeKiroTokens([]byte(body), 200, current, time.Now()); err == nil {
			t.Fatal("invalid token accepted")
		}
	}
}

func TestKiroRefreshRequiresLoginMetadata(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		io.WriteString(w, `{"accessToken":"access","expiresIn":3600}`)
	}))
	defer server.Close()
	client := NewTokenClient(OAuthConfig{Provider: contract.SubscriptionProviderKiro, Issuer: server.URL, HTTPClient: server.Client()})
	for _, test := range []struct{ method, region string }{
		{"", "us-east-1"},
		{"unknown", "us-east-1"},
		{"builder_id", ""},
		{"social", ""},
		{"iam_sso", "invalid"},
	} {
		tokens := AccountTokens{KiroAuthMethod: test.method, Region: test.region, RefreshToken: "refresh", OAuthClientID: "client", OAuthClientSecret: "secret"}
		if _, err := client.RefreshAccount(context.Background(), tokens); !errors.Is(err, ErrInvalidGrant) {
			t.Fatalf("method=%q region=%q: %v", test.method, test.region, err)
		}
	}
	if calls.Load() != 0 {
		t.Fatal("incomplete login metadata reached a guessed token endpoint")
	}
}

func TestKiroAuthorizationDoesNotFollowRedirects(t *testing.T) {
	var leaked atomic.Bool
	destination := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { leaked.Store(true) }))
	defer destination.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, destination.URL, http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	config := OAuthConfig{Provider: contract.SubscriptionProviderKiro, Issuer: server.URL, HTTPClient: server.Client()}.Normalize()
	_, status, err := kiroJSON(context.Background(), config, server.URL, map[string]string{"clientSecret": "secret"})
	if err != nil || status != 307 || leaked.Load() {
		t.Fatalf("redirect followed: status=%d err=%v", status, err)
	}
}
