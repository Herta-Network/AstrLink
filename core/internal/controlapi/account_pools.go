package controlapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/QuantumNous/astrlink/core/contract"
	"github.com/QuantumNous/astrlink/core/internal/storage"
)

const AccountPoolsPath = "/control/v1/account-pools"

type accountPoolResponse struct {
	contract.AccountPool
	Members []contract.AccountPoolMember `json:"members"`
	ETag    string                       `json:"etag"`
	Runtime accountPoolRuntimeResponse   `json:"runtime"`
}

type accountPoolRuntimeResponse struct {
	ActiveRequests    int                       `json:"active_requests"`
	WaitingRequests   int                       `json:"waiting_requests"`
	AvailableAccounts int                       `json:"available_accounts"`
	Members           []accountPoolMemberStatus `json:"members"`
}

type accountPoolMemberStatus struct {
	UsageObservedAt *time.Time                     `json:"usage_observed_at,omitempty"`
	ServiceID       contract.ServiceID             `json:"service_id"`
	State           string                         `json:"state"`
	ActiveRequests  int                            `json:"active_requests"`
	QuotaCooldown   *contract.AccountQuotaCooldown `json:"quota_cooldown,omitempty"`
	ProxySource     string                         `json:"proxy_source"`
}

func (handler *Handler) publicAccountPool(ctx context.Context, record storage.AccountPoolRecord) accountPoolResponse {
	response := accountPoolResponse{AccountPool: record.Pool, Members: record.Members, ETag: record.ETag, Runtime: accountPoolRuntimeResponse{Members: []accountPoolMemberStatus{}}}
	if handler.poolRuntime != nil {
		_, response.Runtime.WaitingRequests = handler.poolRuntime.Scheduler.Counts("", record.Pool.ID)
	}
	for _, member := range record.Members {
		status := accountPoolMemberStatus{ServiceID: member.ServiceID, State: "unknown", ProxySource: "global"}
		if handler.poolRuntime != nil {
			status.ActiveRequests, response.Runtime.WaitingRequests = handler.poolRuntime.Scheduler.Counts(member.ServiceID, record.Pool.ID)
			status.UsageObservedAt = handler.poolRuntime.UsageObservedAt(member.ServiceID)
			if cooldown, cooling := handler.poolRuntime.Scheduler.Cooldown(member.ServiceID); cooling {
				status.QuotaCooldown = &cooldown
			}
		}
		if service, err := handler.serviceStore.GetService(ctx, member.ServiceID); err == nil {
			if record.Pool.Proxy != nil && record.Pool.Proxy.Mode != "inherit" {
				status.ProxySource = "pool"
			}
			if service.Service.Proxy != nil && service.Service.Proxy.Mode != "inherit" {
				status.ProxySource = "account"
			}
			switch {
			case !record.Pool.Enabled || !member.Enabled || !service.Service.Enabled:
				status.State = "disabled"
			case service.Service.Subscription == nil || service.Service.Subscription.Status != contract.SubscriptionStatusConnected:
				status.State = "disconnected"
			case status.QuotaCooldown != nil:
				status.State = "quota_cooldown"
			case service.Service.Subscription.Risk.Blocks(time.Now()):
				status.State = "paused"
			default:
				status.State = "ready"
				response.Runtime.AvailableAccounts++
			}
		}
		response.Runtime.ActiveRequests += status.ActiveRequests
		response.Runtime.Members = append(response.Runtime.Members, status)
	}
	return response
}

func (handler *Handler) accountPoolResource(w http.ResponseWriter, r *http.Request) {
	store, ok := handler.serviceStore.(storage.AccountPoolStore)
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "storage_unavailable", "account pool storage is unavailable")
		return
	}
	if r.URL.RawQuery != "" {
		writeError(w, http.StatusBadRequest, "invalid_query", "account pools do not accept query parameters")
		return
	}
	if r.URL.Path == AccountPoolsPath {
		switch r.Method {
		case http.MethodGet:
			records, err := store.ListAccountPools(r.Context())
			if err != nil {
				handler.writeStoreError(w, err)
				return
			}
			items := make([]accountPoolResponse, 0, len(records))
			for _, record := range records {
				items = append(items, handler.publicAccountPool(r.Context(), record))
			}
			writeJSON(w, http.StatusOK, struct {
				Items []accountPoolResponse `json:"items"`
			}{items})
		case http.MethodPost:
			if !requireMediaType(w, r, "application/json") {
				return
			}
			var input struct {
				Name     string                        `json:"name"`
				Provider contract.SubscriptionProvider `json:"provider"`
				Enabled  json.RawMessage               `json:"enabled,omitempty"`
				Sticky   json.RawMessage               `json:"sticky,omitempty"`
				Queue    json.RawMessage               `json:"queue,omitempty"`
				Proxy    json.RawMessage               `json:"proxy,omitempty"`
			}
			if !decodeControlJSON(w, r, &input) {
				return
			}
			var random [16]byte
			if _, err := rand.Read(random[:]); err != nil {
				handler.writeStoreError(w, err)
				return
			}
			pool := contract.DefaultAccountPool()
			pool.ID, pool.Name, pool.Provider = contract.AccountPoolID("pool_"+hex.EncodeToString(random[:])), input.Name, input.Provider
			if err := applyPoolSettings(&pool, nil, input.Enabled, input.Sticky, input.Queue); err != nil {
				writeError(w, 422, "invalid_account_pool", err.Error())
				return
			}
			mutation, err := applyPoolProxyInput(&pool, input.Proxy)
			if err != nil {
				writeError(w, 422, "invalid_pool_proxy", err.Error())
				return
			}
			record, err := store.CreateAccountPool(r.Context(), pool, mutation)
			if err != nil {
				handler.writeStoreError(w, err)
				return
			}
			w.Header().Set("Location", AccountPoolsPath+"/"+string(pool.ID))
			w.Header().Set("ETag", record.ETag)
			writeJSON(w, http.StatusCreated, handler.publicAccountPool(r.Context(), record))
		default:
			w.Header().Set("Allow", "GET, POST")
			writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "only GET and POST are allowed")
		}
		return
	}
	path := strings.TrimPrefix(r.URL.Path, AccountPoolsPath+"/")
	idText, child, hasChild := strings.Cut(path, "/")
	id := contract.AccountPoolID(idText)
	probe := hasChild && strings.HasPrefix(child, "members/") && strings.HasSuffix(child, "/quota-probe")
	if id.Validate() != nil || hasChild && child != "members" && !probe {
		writeError(w, http.StatusNotFound, "not_found", "account pool path not found")
		return
	}
	current, err := store.GetAccountPool(r.Context(), id)
	if err != nil {
		handler.writeStoreError(w, err)
		return
	}
	if probe {
		if r.Method != http.MethodPost {
			writeMethodNotAllowed(w, http.MethodPost)
			return
		}
		serviceID := contract.ServiceID(strings.TrimSuffix(strings.TrimPrefix(child, "members/"), "/quota-probe"))
		found := false
		for _, member := range current.Members {
			found = found || member.ServiceID == serviceID
		}
		if serviceID.Validate() != nil || !found {
			writeError(w, 404, "not_found", "pool member not found")
			return
		}
		if handler.poolRuntime == nil || !handler.poolRuntime.Scheduler.AllowProbe(serviceID, true) {
			writeError(w, 409, "pool_probe_unavailable", "the account has no available quota recovery probe")
			return
		}
		writeJSON(w, http.StatusOK, struct {
			Armed bool `json:"armed"`
		}{true})
		return
	}
	if hasChild {
		if r.Method != http.MethodPut {
			writeMethodNotAllowed(w, http.MethodPut)
			return
		}
		if !requireMediaType(w, r, "application/json") {
			return
		}
		if !requirePoolETag(w, r) {
			return
		}
		var input struct {
			Members []struct {
				ServiceID             contract.ServiceID `json:"service_id"`
				Enabled               *bool              `json:"enabled"`
				MaxConcurrentRequests int                `json:"max_concurrent_requests"`
			} `json:"members"`
		}
		if !decodeControlJSON(w, r, &input) {
			return
		}
		if input.Members == nil {
			writeError(w, 422, "invalid_pool_members", "members must be an array")
			return
		}
		members := make([]contract.AccountPoolMember, 0, len(input.Members))
		for _, inputMember := range input.Members {
			if inputMember.Enabled == nil {
				writeError(w, 422, "invalid_pool_members", "enabled is required")
				return
			}
			members = append(members, contract.AccountPoolMember{PoolID: id, ServiceID: inputMember.ServiceID, Enabled: *inputMember.Enabled, MaxConcurrentRequests: inputMember.MaxConcurrentRequests})
		}
		record, err := store.ReplaceAccountPoolMembers(r.Context(), id, members, r.Header.Get("If-Match"))
		if err != nil {
			handler.writeStoreError(w, err)
			return
		}
		w.Header().Set("ETag", record.ETag)
		writeJSON(w, http.StatusOK, handler.publicAccountPool(r.Context(), record))
		return
	}
	switch r.Method {
	case http.MethodGet:
		w.Header().Set("ETag", current.ETag)
		writeJSON(w, http.StatusOK, handler.publicAccountPool(r.Context(), current))
	case http.MethodPatch:
		if !requireMediaType(w, r, "application/merge-patch+json") {
			return
		}
		if !requirePoolETag(w, r) {
			return
		}
		var input struct {
			Name    json.RawMessage `json:"name,omitempty"`
			Enabled json.RawMessage `json:"enabled,omitempty"`
			Sticky  json.RawMessage `json:"sticky,omitempty"`
			Queue   json.RawMessage `json:"queue,omitempty"`
			Proxy   json.RawMessage `json:"proxy,omitempty"`
		}
		if !decodeControlJSON(w, r, &input) {
			return
		}
		pool := current.Pool
		if err := applyPoolSettings(&pool, input.Name, input.Enabled, input.Sticky, input.Queue); err != nil {
			writeError(w, 422, "invalid_account_pool", err.Error())
			return
		}
		mutation, err := applyPoolProxyInput(&pool, input.Proxy)
		if err != nil {
			writeError(w, 422, "invalid_pool_proxy", err.Error())
			return
		}
		record, err := store.UpdateAccountPool(r.Context(), pool, mutation, r.Header.Get("If-Match"))
		if err != nil {
			handler.writeStoreError(w, err)
			return
		}
		w.Header().Set("ETag", record.ETag)
		writeJSON(w, http.StatusOK, handler.publicAccountPool(r.Context(), record))
	case http.MethodDelete:
		if !requirePoolETag(w, r) {
			return
		}
		if err := store.DeleteAccountPool(r.Context(), id, r.Header.Get("If-Match")); err != nil {
			handler.writeStoreError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		w.Header().Set("Allow", "GET, PATCH, DELETE")
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "only GET, PATCH and DELETE are allowed")
	}
}

func requirePoolETag(w http.ResponseWriter, r *http.Request) bool {
	if r.Header.Get("If-Match") == "" {
		writeError(w, http.StatusPreconditionRequired, "precondition_required", "If-Match is required")
		return false
	}
	return true
}

func applyPoolProxyInput(pool *contract.AccountPool, raw json.RawMessage) (storage.PoolProxyMutation, error) {
	var mutation storage.PoolProxyMutation
	if raw == nil {
		return mutation, nil
	}
	if isJSONNull(raw) {
		pool.Proxy = nil
		mutation.Present = true
		return mutation, nil
	}
	var input struct {
		Mode       string          `json:"mode"`
		URL        string          `json:"url,omitempty"`
		Credential json.RawMessage `json:"credential,omitempty"`
	}
	if strictUnmarshal(raw, &input) != nil {
		return mutation, fmt.Errorf("invalid proxy configuration")
	}
	proxy := &contract.ServiceProxy{Mode: input.Mode, URL: input.URL}
	if pool.Proxy != nil && proxy.Mode == "custom" {
		proxy.CredentialRef = pool.Proxy.CredentialRef
	}
	if input.Credential != nil {
		mutation.Present = true
		proxy.CredentialRef = ""
		if !isJSONNull(input.Credential) {
			credential := &contract.ProxyCredential{}
			if strictUnmarshal(input.Credential, credential) != nil || credential.Validate() != nil || proxy.Mode != "custom" {
				return mutation, fmt.Errorf("invalid proxy authentication")
			}
			mutation.Credential = credential
		}
	}
	if err := proxy.ValidateForPool(pool.ID); err != nil {
		return mutation, err
	}
	pool.Proxy = proxy
	return mutation, nil
}

func applyPoolSettings(pool *contract.AccountPool, name, enabled, sticky, queue json.RawMessage) error {
	for _, field := range []struct {
		name   string
		raw    json.RawMessage
		target any
	}{{"name", name, &pool.Name}, {"enabled", enabled, &pool.Enabled}, {"sticky", sticky, &pool.Sticky}, {"queue", queue, &pool.Queue}} {
		if field.raw != nil && (isJSONNull(field.raw) || strictUnmarshal(field.raw, field.target) != nil) {
			return fmt.Errorf("invalid %s", field.name)
		}
	}
	return nil
}
