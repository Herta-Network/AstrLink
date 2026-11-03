package controlapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/QuantumNous/astrlink/core/contract"
	"github.com/QuantumNous/astrlink/core/internal/networkproxy"
	"github.com/QuantumNous/astrlink/core/internal/secretstore"
	"github.com/QuantumNous/astrlink/core/internal/storage"
)

const ServiceProxyProbesPath = "/control/v1/service-proxy-probes"

// probeServiceProxy uses only the draft proxy credentials. It neither saves the
// draft nor sends the service's API credentials or follows upstream redirects.
func (handler *Handler) probeServiceProxy(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		writeMethodNotAllowed(writer, http.MethodPost)
		return
	}
	var input struct {
		ServiceID contract.ServiceID     `json:"service_id,omitempty"`
		PoolID    contract.AccountPoolID `json:"pool_id,omitempty"`
		Proxy     json.RawMessage        `json:"proxy"`
		TargetURL string                 `json:"target_url"`
	}
	if !decodeControlJSON(writer, request, &input) {
		return
	}
	if input.ServiceID != "" && input.PoolID != "" {
		writeError(writer, http.StatusUnprocessableEntity, "invalid_service_proxy", "choose either a service or a pool")
		return
	}
	target, err := url.Parse(input.TargetURL)
	if err != nil || len(input.TargetURL) > 2048 || target == nil || target.Hostname() == "" || (target.Scheme != "http" && target.Scheme != "https") || target.User != nil || target.RawQuery != "" || target.ForceQuery || target.Fragment != "" {
		writeError(writer, http.StatusUnprocessableEntity, "invalid_proxy_target", "proxy test target must be an HTTP or HTTPS URL without authentication, query or fragment")
		return
	}
	service := contract.Service{ID: "service_proxy_probe"}
	if input.ServiceID != "" {
		if input.ServiceID.Validate() != nil {
			writeError(writer, http.StatusUnprocessableEntity, "invalid_service_id", "service_id is invalid")
			return
		}
		record, err := handler.serviceStore.GetService(request.Context(), input.ServiceID)
		if err != nil {
			handler.writeStoreError(writer, err)
			return
		}
		service = record.Service
	}
	var proxy struct {
		Mode string `json:"mode"`
	}
	if json.Unmarshal(input.Proxy, &proxy) != nil || proxy.Mode != "custom" {
		writeError(writer, http.StatusUnprocessableEntity, "invalid_service_proxy", "proxy test requires a custom proxy")
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), 10*time.Second)
	defer cancel()
	if input.PoolID != "" {
		ctx, err = handler.bindDraftPoolProxy(ctx, input.PoolID, input.Proxy)
	} else {
		ctx, err = handler.bindDraftProxy(ctx, service, input.Proxy)
	}
	if err != nil {
		writeError(writer, http.StatusUnprocessableEntity, "invalid_service_proxy", err.Error())
		return
	}
	probe, err := http.NewRequestWithContext(ctx, http.MethodHead, target.String(), nil)
	if err != nil {
		writeError(writer, http.StatusUnprocessableEntity, "invalid_proxy_target", "invalid proxy test target")
		return
	}
	probe.Header.Set("User-Agent", "")
	client := networkproxy.WrapClient(&http.Client{
		Timeout:       10 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	})
	defer client.CloseIdleConnections()
	started := time.Now()
	response, err := client.Do(probe)
	if err != nil {
		// Transport errors can contain proxy userinfo; never return or log them.
		writeError(writer, http.StatusBadGateway, "proxy_connection_failed", "proxy connection failed; check the address, authentication and target")
		return
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusProxyAuthRequired {
		writeError(writer, http.StatusBadGateway, "proxy_authentication_failed", "proxy authentication failed")
		return
	}
	writeJSON(writer, http.StatusOK, struct {
		LatencyMS  int64 `json:"latency_ms"`
		StatusCode int   `json:"status_code"`
	}{time.Since(started).Milliseconds(), response.StatusCode})
}

func applyServiceProxyInput(service *contract.Service, mutation *storage.CredentialMutation, raw json.RawMessage) error {
	if raw == nil {
		return nil
	}
	if isJSONNull(raw) {
		service.Proxy = nil
		mutation.ProxyPresent = true
		return nil
	}
	var input struct {
		Mode       string          `json:"mode"`
		URL        string          `json:"url,omitempty"`
		Credential json.RawMessage `json:"credential,omitempty"`
	}
	if strictUnmarshal(raw, &input) != nil {
		return fmt.Errorf("invalid proxy configuration")
	}
	proxy := &contract.ServiceProxy{Mode: input.Mode, URL: input.URL}
	if service.Proxy != nil && proxy.Mode == "custom" {
		proxy.CredentialRef = service.Proxy.CredentialRef
	}
	if input.Credential != nil {
		mutation.ProxyPresent = true
		proxy.CredentialRef = ""
		if !isJSONNull(input.Credential) {
			credential := &contract.ProxyCredential{}
			if strictUnmarshal(input.Credential, credential) != nil || credential.Validate() != nil || proxy.Mode != "custom" {
				return fmt.Errorf("invalid proxy authentication")
			}
			mutation.Proxy = credential
		}
	}
	if err := proxy.Validate(service.ID); err != nil {
		return err
	}
	service.Proxy = proxy
	return nil
}

func (handler *Handler) bindDraftProxy(ctx context.Context, service contract.Service, raw json.RawMessage) (context.Context, error) {
	mutation := storage.CredentialMutation{}
	if err := applyServiceProxyInput(&service, &mutation, raw); err != nil {
		return ctx, err
	}
	if mutation.ProxyPresent && service.Proxy != nil && service.Proxy.Mode == "custom" {
		return networkproxy.BindConfig(ctx, service.ID, service.Proxy, mutation.Proxy)
	}
	secrets, _ := handler.serviceStore.(secretstore.SecretStore)
	return networkproxy.Bind(ctx, service, secrets)
}

func (handler *Handler) bindDraftPoolProxy(ctx context.Context, id contract.AccountPoolID, raw json.RawMessage) (context.Context, error) {
	if err := id.Validate(); err != nil {
		return ctx, err
	}
	pools, ok := handler.serviceStore.(storage.AccountPoolStore)
	if !ok {
		return ctx, fmt.Errorf("account pool storage unavailable")
	}
	record, err := pools.GetAccountPool(ctx, id)
	if err != nil {
		return ctx, err
	}
	mutation, err := applyPoolProxyInput(&record.Pool, raw)
	if err != nil {
		return ctx, err
	}
	config := record.Pool.Proxy
	credential := mutation.Credential
	if !mutation.Present && config != nil && config.CredentialRef != "" {
		secrets, ok := handler.serviceStore.(secretstore.SecretStore)
		if !ok {
			return ctx, fmt.Errorf("pool proxy credential unavailable")
		}
		value, err := secrets.Get(ctx, secretstore.Ref(config.CredentialRef))
		if err != nil {
			return ctx, fmt.Errorf("pool proxy credential unavailable")
		}
		defer clear(value)
		credential = &contract.ProxyCredential{}
		if json.Unmarshal(value, credential) != nil || credential.Validate() != nil {
			return ctx, fmt.Errorf("invalid pool proxy credential")
		}
	}
	if config != nil {
		resolved := *config
		resolved.CredentialRef = ""
		config = &resolved
	}
	return networkproxy.BindConfig(ctx, "service_proxy_probe", config, credential)
}
