package accountauth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/astrlink/core/contract"
)

const kiroEnterpriseProfile = "arn:aws:codewhisperer:eu-central-1:123456789012:profile/test-profile"

func TestKiroSocialAndEnterpriseBrowserAuthorization(t *testing.T) {
	for _, method := range []string{"social", "iam_sso"} {
		for _, manual := range []bool{false, true} {
			t.Run(method+map[bool]string{false: "/callback", true: "/manual"}[manual], func(t *testing.T) {
				var challenge, redirect string
				var exchanges atomic.Int32
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					for name, values := range r.Header {
						if strings.Contains(strings.ToLower(name+strings.Join(values, "")), "astrlink") {
							t.Errorf("branded authentication header: %s", name)
						}
					}
					var payload map[string]any
					if json.NewDecoder(r.Body).Decode(&payload) != nil {
						t.Error("invalid JSON auth request")
					}
					w.Header().Set("Content-Type", "application/json")
					switch r.URL.Path {
					case "/client/register":
						if payload["issuerUrl"] != "https://company.awsapps.com/start" || payload["clientName"] != "Kiro" {
							t.Errorf("enterprise registration: %v", payload)
						}
						io.WriteString(w, `{"clientId":"public-client-id","clientSecret":"client-secret","clientSecretExpiresAt":4102444800}`)
					case "/oauth/token", "/token":
						exchanges.Add(1)
						code, verifier, uri := payload["code"], payload["code_verifier"], payload["redirect_uri"]
						if method == "iam_sso" {
							verifier, uri = payload["codeVerifier"], payload["redirectUri"]
							if payload["clientId"] != "public-client-id" || payload["clientSecret"] != "client-secret" || payload["grantType"] != "authorization_code" {
								t.Error("missing enterprise grant")
							}
						}
						v, _ := verifier.(string)
						hash := sha256.Sum256([]byte(v))
						if code != "code-secret" || uri != redirect || base64.RawURLEncoding.EncodeToString(hash[:]) != challenge {
							t.Errorf("PKCE/redirect mismatch: %v", payload)
						}
						if method == "social" {
							io.WriteString(w, `{"accessToken":"access-secret","refreshToken":"refresh-secret","profileArn":"`+kiroEnterpriseProfile+`","expiresIn":3600}`)
						} else {
							io.WriteString(w, `{"accessToken":"access-secret","refreshToken":"refresh-secret","expiresIn":3600}`)
						}
					case "/ListAvailableProfiles":
						if r.Header.Get("Authorization") != "Bearer access-secret" {
							t.Error("profile lookup unauthenticated")
						}
						io.WriteString(w, `{"profiles":[{"arn":"`+kiroEnterpriseProfile+`"}]}`)
					case "/refreshToken":
						if method != "social" || len(payload) != 1 || payload["refreshToken"] != "refresh-secret" {
							t.Errorf("wrong social refresh: %v", payload)
						}
						io.WriteString(w, `{"accessToken":"access-rotated","refreshToken":"refresh-rotated","expiresIn":7200}`)
					default:
						t.Errorf("unexpected request: %s", r.URL.Path)
						w.WriteHeader(404)
					}
				}))
				defer upstream.Close()
				port := 0
				config := OAuthConfig{Provider: contract.SubscriptionProviderKiro, Issuer: upstream.URL, APIBaseURL: upstream.URL, KiroSocialBaseURL: upstream.URL, KiroPortalURL: upstream.URL + "/signin", KiroCallbackPort: &port, NoLoopbackCallback: manual, HTTPClient: upstream.Client()}
				// A manual social login still uses the provider's registered origin.
				if manual {
					config.KiroCallbackPort = nil
				}
				stored := make(chan AccountTokens, 1)
				manager := NewSessionManager(config, NewMemoryCredentialStore(), func(_ context.Context, _ contract.AuthorizationSession, tokens AccountTokens) error {
					stored <- tokens
					return nil
				})
				defer manager.CancelAllForService("service_login")
				options := contract.KiroAuthorization{Method: method}
				if method == "iam_sso" {
					options.Region = "eu-west-1"
					options.StartURL = "https://company.awsapps.com/start"
				}
				session, err := manager.Begin(context.Background(), "service_login", contract.AuthorizationFlowBrowser, &options)
				if err != nil {
					t.Fatal(err)
				}
				authURL, err := url.Parse(session.AuthorizationURL)
				if err != nil {
					t.Fatal(err)
				}
				query := authURL.Query()
				challenge, redirect = query.Get("code_challenge"), query.Get("redirect_uri")
				if challenge == "" || query.Get("state") == "" || query.Get("code_challenge_method") != "S256" {
					t.Fatal("missing OAuth protections")
				}
				if method == "social" && query.Get("redirect_from") != "KiroIDE" {
					t.Fatal("social client identity missing")
				}
				raw, _ := json.Marshal(session)
				for _, secret := range []string{"client-secret", "refresh-secret", "code-secret"} {
					if strings.Contains(string(raw), secret) {
						t.Fatal("secret in public authorization session")
					}
				}
				callback := redirect
				if method == "social" {
					callback += "/oauth/callback"
				}
				callback += "?code=code-secret&state=" + url.QueryEscape(query.Get("state"))
				bad := strings.Replace(callback, "state=", "state=bad-", 1)
				if _, err := manager.CompleteCode(context.Background(), "service_login", session.ID, bad); !errors.Is(err, ErrStateMismatch) {
					t.Fatalf("accepted wrong state: %v", err)
				}
				if exchanges.Load() != 0 {
					t.Fatal("bad callback reached token endpoint")
				}
				if manual {
					if session.Flow != contract.AuthorizationFlowCode {
						t.Fatal("server login must expose manual callback")
					}
					if _, err := manager.CompleteCode(context.Background(), "service_login", session.ID, callback); err != nil {
						t.Fatal(err)
					}
				} else {
					response, err := http.Get(callback)
					if err != nil {
						t.Fatal(err)
					}
					_, _ = io.Copy(io.Discard, response.Body)
					_ = response.Body.Close()
				}
				var tokens AccountTokens
				select {
				case tokens = <-stored:
				case <-time.After(3 * time.Second):
					t.Fatal("browser login did not finish")
				}
				if tokens.KiroAuthMethod != method || tokens.ProjectID != kiroEnterpriseProfile || tokens.RefreshToken != "refresh-secret" {
					t.Fatalf("wrong credentials: method=%s profile=%s", tokens.KiroAuthMethod, tokens.ProjectID)
				}
				if method == "iam_sso" && tokens.Region != "eu-west-1" {
					t.Fatal("profile discovery changed OIDC region")
				}
				if got := KiroAPIBaseURL(DefaultKiroAPIBaseURL, tokens); got != "https://q.eu-central-1.amazonaws.com" {
					t.Fatalf("data plane=%s", got)
				}
				if _, err := manager.CompleteCode(context.Background(), "service_login", session.ID, callback); !errors.Is(err, ErrSessionNotPending) {
					t.Fatalf("callback replay accepted: %v", err)
				}
				if method == "social" {
					refreshed, err := NewTokenClient(config).RefreshAccount(context.Background(), tokens)
					if err != nil || refreshed.RefreshToken != "refresh-rotated" || refreshed.KiroAuthMethod != "social" || refreshed.ProjectID != tokens.ProjectID {
						t.Fatalf("social refresh failed: %v", err)
					}
				}
			})
		}
	}
}

func TestKiroCancelledBrowserLoginCannotPersist(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		select {
		case <-r.Context().Done():
		case <-release:
		}
		io.WriteString(w, `{"accessToken":"a","refreshToken":"r","profileArn":"p","expiresIn":3600}`)
	}))
	t.Cleanup(func() { close(release); server.Close() })
	var persisted atomic.Bool
	config := OAuthConfig{Provider: contract.SubscriptionProviderKiro, KiroSocialBaseURL: server.URL, KiroPortalURL: server.URL, NoLoopbackCallback: true, HTTPClient: server.Client()}
	manager := NewSessionManager(config, NewMemoryCredentialStore(), func(context.Context, contract.AuthorizationSession, AccountTokens) error {
		persisted.Store(true)
		return nil
	})
	session, err := manager.Begin(context.Background(), "service_cancel", contract.AuthorizationFlowBrowser, &contract.KiroAuthorization{Method: "social"})
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(session.AuthorizationURL)
	callback := u.Query().Get("redirect_uri") + "/oauth/callback?code=c&state=" + u.Query().Get("state")
	done := make(chan error, 1)
	go func() {
		_, err := manager.CompleteCode(context.Background(), "service_cancel", session.ID, callback)
		done <- err
	}()
	select {
	case <-entered:
	case err := <-done:
		t.Fatalf("exchange ended before upstream: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("exchange did not reach upstream")
	}
	if _, err := manager.Cancel(context.Background(), "service_cancel"); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancelled exchange succeeded")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("cancel did not stop token exchange")
	}
	if persisted.Load() {
		t.Fatal("cancelled login persisted credentials")
	}
}

func TestKiroEnterpriseDeviceLoginKeepsAuthenticationRegionAcrossRefresh(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/client/register":
			if body["issuerUrl"] != "https://company.awsapps.com/start" {
				t.Error("wrong enterprise issuer")
			}
			io.WriteString(w, `{"clientId":"enterprise-client","clientSecret":"private-registration"}`)
		case "/device_authorization":
			if body["startUrl"] != "https://company.awsapps.com/start" {
				t.Error("wrong enterprise start URL")
			}
			io.WriteString(w, `{"deviceCode":"private-device","userCode":"SSO-CODE","verificationUri":"https://company.awsapps.com/device","interval":1,"expiresIn":600}`)
		case "/token":
			if body["clientId"] != "enterprise-client" || body["clientSecret"] != "private-registration" {
				t.Error("lost registered client")
			}
			if body["grantType"] == "refresh_token" {
				if body["refreshToken"] != "refresh-first" {
					t.Error("wrong refresh token")
				}
				io.WriteString(w, `{"accessToken":"access-second","refreshToken":"refresh-second","expiresIn":7200}`)
			} else {
				io.WriteString(w, `{"accessToken":"access-first","refreshToken":"refresh-first","expiresIn":3600}`)
			}
		case "/ListAvailableProfiles":
			io.WriteString(w, `{"profiles":[{"arn":"`+kiroEnterpriseProfile+`"}]}`)
		default:
			t.Errorf("unexpected endpoint %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	config := OAuthConfig{Provider: contract.SubscriptionProviderKiro, Issuer: server.URL, APIBaseURL: server.URL, HTTPClient: server.Client(), DevicePollMinInterval: time.Millisecond, DevicePollMaxInterval: time.Millisecond}
	completed := make(chan AccountTokens, 1)
	manager := NewSessionManager(config, NewMemoryCredentialStore(), func(_ context.Context, _ contract.AuthorizationSession, tokens AccountTokens) error {
		completed <- tokens
		return nil
	})
	defer manager.CancelAllForService("service_sso")
	_, err := manager.Begin(context.Background(), "service_sso", contract.AuthorizationFlowDeviceCode, &contract.KiroAuthorization{Method: "iam_sso", Region: "eu-west-1", StartURL: "https://company.awsapps.com/start"})
	if err != nil {
		t.Fatal(err)
	}
	var tokens AccountTokens
	select {
	case tokens = <-completed:
	case <-time.After(3 * time.Second):
		t.Fatal("SSO device login did not finish")
	}
	if tokens.KiroAuthMethod != "iam_sso" || tokens.Region != "eu-west-1" || tokens.ProjectID != kiroEnterpriseProfile {
		t.Fatal("SSO account metadata lost")
	}
	refreshed, err := NewTokenClient(config).RefreshAccount(context.Background(), tokens)
	if err != nil || refreshed.Region != "eu-west-1" || refreshed.ProjectID != kiroEnterpriseProfile || refreshed.RefreshToken != "refresh-second" {
		t.Fatalf("SSO refresh failed: %v", err)
	}
	regional, err := kiroConfigForRegion(OAuthConfig{Provider: contract.SubscriptionProviderKiro}.Normalize(), refreshed.Region)
	if err != nil || regional.TokenURL != "https://oidc.eu-west-1.amazonaws.com/token" {
		t.Fatalf("refresh region mixed with service region: %s %v", regional.TokenURL, err)
	}
}
