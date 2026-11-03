package accountauth

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/QuantumNous/astrlink/core/contract"
	"github.com/QuantumNous/astrlink/core/internal/networkproxy"
)

const DefaultKiroSocialBaseURL = "https://prod.us-east-1.auth.desktop.kiro.dev"
const DefaultKiroPortalURL = "https://app.kiro.dev/signin"

type kiroAuthorizationState struct {
	config OAuthConfig
	tokens AccountTokens
}

func (manager *SessionManager) beginKiroAuthorization(ctx context.Context, id contract.ServiceID, flow contract.AuthorizationFlow, options contract.KiroAuthorization) (contract.AuthorizationSession, error) {
	options, err := options.Normalize(flow)
	if err != nil {
		return contract.AuthorizationSession{}, err
	}
	if flow == contract.AuthorizationFlowDeviceCode {
		return manager.beginPolledDeviceAuthorization(ctx, id, func(ctx context.Context) (polledDeviceAuthorization, error) {
			return manager.requestKiroDeviceAuthorization(ctx, options)
		}, manager.pollKiroDeviceAuthorizationOnce, "Kiro")
	}
	manager.mu.Lock()
	err = manager.canStartLocked(id)
	manager.mu.Unlock()
	if err != nil {
		return contract.AuthorizationSession{}, err
	}
	config, err := kiroConfigForRegion(manager.config, options.Region)
	if err != nil {
		return contract.AuthorizationSession{}, err
	}
	state, err := generateState()
	if err != nil {
		return contract.AuthorizationSession{}, err
	}
	pkce, err := generatePKCE()
	if err != nil {
		return contract.AuthorizationSession{}, err
	}
	manual := flow == contract.AuthorizationFlowCode || config.NoLoopbackCallback
	port := 3128
	if options.Method == "iam_sso" {
		port = 0
	}
	if config.KiroCallbackPort != nil {
		port = *config.KiroCallbackPort
	}
	var listener net.Listener
	if !manual {
		listener, err = net.Listen("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
		if err != nil {
			return contract.AuthorizationSession{}, fmt.Errorf("Kiro callback port is unavailable; use manual callback authorization")
		}
		defer func() {
			if listener != nil {
				_ = listener.Close()
			}
		}()
		port = listener.Addr().(*net.TCPAddr).Port
	}
	redirect := "http://localhost:" + strconv.Itoa(port)
	if options.Method == "iam_sso" {
		if manual && port == 0 {
			redirect = "http://127.0.0.1"
		}
		redirect += "/oauth/callback"
	}
	private := &kiroAuthorizationState{config: config, tokens: AccountTokens{KiroAuthMethod: options.Method, Region: options.Region}}
	var authURL string
	if options.Method == "social" {
		base := config.KiroPortalURL
		if base == "" {
			base = DefaultKiroPortalURL
		}
		u, parseErr := url.Parse(base)
		if parseErr != nil {
			return contract.AuthorizationSession{}, fmt.Errorf("invalid Kiro login endpoint")
		}
		q := u.Query()
		q.Set("state", state)
		q.Set("code_challenge", pkce.Challenge)
		q.Set("code_challenge_method", "S256")
		q.Set("redirect_uri", redirect)
		q.Set("redirect_from", "KiroIDE")
		u.RawQuery = q.Encode()
		authURL = u.String()
	} else {
		body, status, callErr := kiroJSON(ctx, config, strings.TrimRight(config.Issuer, "/")+"/client/register", map[string]any{"clientName": "Kiro", "clientType": "public", "scopes": kiroScopes, "grantTypes": []string{"authorization_code", "refresh_token"}, "issuerUrl": options.StartURL, "redirectUris": []string{redirect}})
		if callErr != nil || status != 200 {
			return contract.AuthorizationSession{}, fmt.Errorf("Kiro enterprise client registration failed")
		}
		private.tokens, err = kiroRegistration(body, private.tokens, config.Now())
		if err != nil {
			return contract.AuthorizationSession{}, err
		}
		q := url.Values{"response_type": {"code"}, "client_id": {private.tokens.OAuthClientID}, "redirect_uri": {redirect}, "scopes": {strings.Join(kiroScopes, ",")}, "state": {state}, "code_challenge": {pkce.Challenge}, "code_challenge_method": {"S256"}}
		authURL = strings.TrimRight(config.Issuer, "/") + "/authorize?" + q.Encode()
	}
	sessionID, err := manager.newID()
	if err != nil {
		return contract.AuthorizationSession{}, err
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if err := manager.canStartLocked(id); err != nil {
		return contract.AuthorizationSession{}, err
	}
	actualFlow := contract.AuthorizationFlowBrowser
	if manual {
		actualFlow = contract.AuthorizationFlowCode
	}
	now := config.Now().UTC()
	public := contract.AuthorizationSession{ID: sessionID, Provider: contract.SubscriptionProviderKiro, ServiceID: id, Flow: actualFlow, Status: contract.AuthorizationSessionStatusPending, AuthorizationURL: authURL, CreatedAt: now, UpdatedAt: now, ExpiresAt: now.Add(config.SessionTTL)}
	sessionCtx, cancel := context.WithCancel(networkproxy.Copy(context.Background(), ctx))
	secrets := &sessionSecrets{kiro: private, proxyContext: sessionCtx, state: state, pkce: pkce, redirectURI: redirect, cancel: cancel, listener: listener}
	manager.sessions[sessionID] = &trackedSession{public: public, secrets: secrets}
	manager.byService[id] = sessionID
	if listener != nil {
		server := &http.Server{ReadHeaderTimeout: 5 * time.Second, Handler: manager.kiroCallbackHandler(id, sessionID)}
		owned := listener
		go func() { _ = server.Serve(owned) }()
		listener = nil
	}
	go manager.expireAfter(sessionCtx, sessionID, config.SessionTTL)
	return public, nil
}

func (manager *SessionManager) kiroCallbackHandler(serviceID contract.ServiceID, id contract.AuthorizationSessionID) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Referrer-Policy", "no-referrer")
		if r.Method != http.MethodGet || (r.URL.Path != "/oauth/callback" && r.URL.Path != "/signin/callback") {
			http.NotFound(w, r)
			return
		}
		_, err := manager.completeKiroCode(r.Context(), serviceID, id, "http://"+r.Host+r.URL.RequestURI())
		if err != nil {
			writeCallbackPage(w, false, "Kiro authorization could not be completed. Check the sign-in session and try again.")
			return
		}
		writeCallbackPage(w, true, "Kiro is connected. You can close this window.")
	})
}

// A manual callback is also available when the browser runs on another host.
// State, exact callback origin/path and an exchange claim prevent CSRF/replay.
func (manager *SessionManager) completeKiroCode(ctx context.Context, serviceID contract.ServiceID, id contract.AuthorizationSessionID, callback string) (contract.AuthorizationSession, error) {
	manager.mu.Lock()
	manager.expirePendingLocked()
	session := manager.sessions[id]
	if session == nil || session.public.ServiceID != serviceID || manager.byService[serviceID] != id || session.public.Status != contract.AuthorizationSessionStatusPending || session.completing || session.exchanging || session.secrets == nil || session.secrets.kiro == nil {
		manager.mu.Unlock()
		return contract.AuthorizationSession{}, ErrSessionNotPending
	}
	secrets := session.secrets
	u, err := url.Parse(strings.TrimSpace(callback))
	expected, _ := url.Parse(secrets.redirectURI)
	if err != nil || len(callback) > 8192 || u.User != nil || u.Fragment != "" || u.Scheme != expected.Scheme || u.Host != expected.Host || (u.Path != "/oauth/callback" && u.Path != "/signin/callback") {
		manager.mu.Unlock()
		return contract.AuthorizationSession{}, ErrStateMismatch
	}
	q := u.Query()
	if len(q["state"]) == 1 && subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(secrets.state)) == 1 && q.Get("error") != "" {
		manager.mu.Unlock()
		manager.failSession(id, &contract.SubscriptionError{Code: ErrCodeCallbackFailed, Message: "Kiro sign-in was declined"})
		return contract.AuthorizationSession{}, fmt.Errorf("Kiro sign-in was declined")
	}
	if len(q["state"]) != 1 || subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(secrets.state)) != 1 || len(q["code"]) != 1 || q.Get("code") == "" {
		manager.mu.Unlock()
		return contract.AuthorizationSession{}, ErrStateMismatch
	}
	session.exchanging = true
	manager.mu.Unlock()
	defer func() { manager.mu.Lock(); session.exchanging = false; manager.mu.Unlock() }()
	ctx, cancel := context.WithTimeout(networkproxy.Copy(ctx, secrets.proxyContext), 30*time.Second)
	defer cancel()
	stopCancel := context.AfterFunc(secrets.proxyContext, cancel)
	defer stopCancel()
	private := secrets.kiro
	config := private.config
	endpoint := config.TokenURL
	payload := map[string]string{"clientId": private.tokens.OAuthClientID, "clientSecret": private.tokens.OAuthClientSecret, "grantType": "authorization_code", "redirectUri": secrets.redirectURI, "code": q.Get("code"), "codeVerifier": secrets.pkce.Verifier}
	if private.tokens.KiroAuthMethod == "social" {
		endpoint = kiroSocialBase(config) + "/oauth/token"
		payload = map[string]string{"code": q.Get("code"), "code_verifier": secrets.pkce.Verifier, "redirect_uri": secrets.redirectURI}
	}
	body, status, err := kiroJSON(ctx, config, endpoint, payload)
	if err != nil {
		manager.failSession(id, &contract.SubscriptionError{Code: ErrCodeCallbackFailed, Message: "Kiro authorization code exchange failed"})
		return contract.AuthorizationSession{}, err
	}
	tokens, err := decodeKiroTokens(body, status, private.tokens, config.Now())
	if err == nil && tokens.ProjectID == "" {
		err = resolveKiroProfile(ctx, config, &tokens)
	}
	if err != nil {
		manager.failSession(id, &contract.SubscriptionError{Code: ErrCodeCallbackFailed, Message: "Kiro authorization or profile lookup failed"})
		return contract.AuthorizationSession{}, fmt.Errorf("Kiro authorization or profile lookup failed")
	}
	if err := manager.completeSession(ctx, id, tokens); err != nil {
		return contract.AuthorizationSession{}, err
	}
	public, _ := manager.Get(serviceID)
	return public, nil
}

var kiroScopes = []string{"codewhisperer:completions", "codewhisperer:analysis", "codewhisperer:conversations", "codewhisperer:transformations", "codewhisperer:taskassist"}

func kiroSocialBase(config OAuthConfig) string {
	if config.KiroSocialBaseURL != "" {
		return strings.TrimRight(config.KiroSocialBaseURL, "/")
	}
	return DefaultKiroSocialBaseURL
}

func kiroRegistration(body []byte, tokens AccountTokens, now time.Time) (AccountTokens, error) {
	var registration struct {
		ClientID     string `json:"clientId"`
		ClientSecret string `json:"clientSecret"`
		Expires      int64  `json:"clientSecretExpiresAt"`
	}
	if json.Unmarshal(body, &registration) != nil || registration.ClientID == "" || registration.ClientSecret == "" {
		return tokens, fmt.Errorf("invalid Kiro client registration")
	}
	tokens.OAuthClientID = registration.ClientID
	tokens.OAuthClientSecret = registration.ClientSecret
	if registration.Expires > 0 {
		tokens.OAuthClientExpiresAt = time.Unix(registration.Expires, 0)
		if !tokens.OAuthClientExpiresAt.After(now) {
			return tokens, fmt.Errorf("Kiro client registration has expired")
		}
	}
	return tokens, nil
}
