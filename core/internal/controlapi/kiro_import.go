package controlapi

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"time"

	"github.com/QuantumNous/astrlink/core/contract"
	"github.com/QuantumNous/astrlink/core/internal/accountauth"
	"github.com/QuantumNous/astrlink/core/internal/networkproxy"
	"github.com/QuantumNous/astrlink/core/internal/storage"
	"github.com/QuantumNous/astrlink/core/internal/subscription"
)

// Retrying creation from the same export addresses the same service, even
// after the provider rotated its refresh token. No file metadata is trusted.
func kiroImportServiceID(refresh string) contract.ServiceID {
	hash := sha256.Sum256([]byte("kiro-import\x00" + refresh))
	return contract.ServiceID("service_" + hex.EncodeToString(hash[:16]))
}

func (handler *Handler) importExistingKiro(w http.ResponseWriter, r *http.Request, id contract.ServiceID) {
	if r.Method != http.MethodPost {
		writeMethodNotAllowed(w, http.MethodPost)
		return
	}
	if !requireMediaType(w, r) {
		return
	}
	var grant contract.KiroSocialGrant
	if !decodeControlJSON(w, r, &grant) {
		return
	}
	record, err := handler.serviceStore.GetService(r.Context(), id)
	if err != nil {
		handler.writeStoreError(w, err)
		return
	}
	handler.importKiroGrant(w, r, record.Service, storage.CredentialMutation{}, grant, record.ETag)
}

func (handler *Handler) importKiroGrant(w http.ResponseWriter, r *http.Request, service contract.Service, mutation storage.CredentialMutation, grant contract.KiroSocialGrant, etag string) {
	if service.Kind != contract.ServiceKindKiroSubscription || !grant.Valid() {
		writeError(w, http.StatusUnprocessableEntity, "invalid_kiro_import", "choose a Google or GitHub Kiro credential file")
		return
	}
	if handler.subscriptions == nil {
		writeError(w, http.StatusServiceUnavailable, "subscription_unavailable", "subscription services are unavailable")
		return
	}
	ctx := r.Context()
	var err error
	if etag == "" {
		if _, err := handler.serviceStore.GetService(ctx, service.ID); err == nil {
			writeError(w, http.StatusConflict, "kiro_already_imported", "this Kiro credential file has already been imported")
			return
		} else if !errors.Is(err, storage.ErrNotFound) {
			handler.writeStoreError(w, err)
			return
		}
		ctx, err = networkproxy.BindConfig(ctx, service.ID, service.Proxy, mutation.Proxy)
	} else {
		ctx, err = handler.subscriptions.ProxyContext(ctx, service.ID)
	}
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, "invalid_service_proxy", "service proxy is unavailable")
		return
	}
	var result storage.ServiceRecord
	var storeErr error
	err = handler.subscriptions.ImportKiro(ctx, service.ID, grant, func(tokens accountauth.AccountTokens) error {
		secret, err := tokens.MarshalSecret()
		if err != nil {
			return err
		}
		defer clear(secret)
		now := time.Now().UTC()
		service.Subscription = &contract.SubscriptionConnection{
			Provider: contract.SubscriptionProviderKiro, Status: contract.SubscriptionStatusConnected,
			Mode: service.Subscription.Mode, AuthorizationBoundary: handler.subscriptions.AuthorizationBoundary(),
			CredentialRef: accountauth.CredentialRefFor(service.ID), TokenExpiresAt: &tokens.ExpiresAt, LastRefreshAt: &now,
		}
		mutation.Subscription = secret
		if etag == "" {
			result, storeErr = handler.serviceStore.CreateService(ctx, service, mutation)
		} else {
			result, storeErr = handler.serviceStore.UpdateService(ctx, service, mutation, etag)
		}
		return storeErr
	})
	if storeErr != nil {
		handler.writeStoreError(w, storeErr)
		return
	}
	if err != nil {
		if errors.Is(err, subscription.ErrKiroImportBusy) {
			writeError(w, http.StatusConflict, "authorization_busy", "another authorization is in progress")
			return
		}
		if errors.Is(err, accountauth.ErrInvalidGrant) {
			writeError(w, http.StatusUnprocessableEntity, "kiro_grant_rejected", "Kiro rejected the refresh token")
			return
		}
		writeError(w, http.StatusBadGateway, "kiro_import_failed", "Kiro token refresh or profile lookup failed")
		return
	}
	status := http.StatusOK
	if etag == "" {
		status = http.StatusCreated
		w.Header().Set("Location", ServicesPath+"/"+string(service.ID))
	}
	w.Header().Set("ETag", result.ETag)
	writeJSON(w, status, handler.publicService(result.Service))
}
