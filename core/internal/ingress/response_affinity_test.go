package ingress

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/astrlink/core/contract"
	"github.com/QuantumNous/astrlink/core/internal/endpoint"
	"github.com/QuantumNous/astrlink/core/internal/providerapi"
	"github.com/QuantumNous/astrlink/core/internal/storage"
	"github.com/QuantumNous/astrlink/core/internal/transport"
)

func TestResponseAffinityFollowsActualBackupAndIsolatesPrincipals(t *testing.T) {
	policy := contract.DefaultFailurePolicy()
	policy.InitialDelayMS = 0
	policy.MaxRetries = 0
	candidates := recoveryCandidates(2, policy, contract.DefaultFailoverPolicy())
	for i := range candidates {
		candidates[i].Endpoint.Capabilities = []contract.Capability{{Protocol: contract.ProtocolOpenAIResponses, Mode: contract.CapabilityModeNative}}
		candidates[i].UpstreamModel = []string{"model-a", "model-b"}[i]
		candidates[i].UpstreamProtocol = contract.ProtocolOpenAIResponses
	}
	var attempts []string
	handler := NewWithDependencies(Dependencies{Resolver: candidateResolver{candidates: candidates}, Forwarder: transport.New(roundTripFunc(func(request *http.Request) (*http.Response, error) {
		attempts = append(attempts, request.URL.Host)
		if request.URL.Host == "A.example" {
			return jsonResponse(503, `{"error":{"message":"unavailable"}}`), nil
		}
		return jsonResponse(200, `{"id":"resp_actual_b","model":"model-b","output":[]}`), nil
	}))})
	response := httptest.NewRecorder()
	request := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{"model":"public","input":"hello"}`))
	request.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(response, request)
	if response.Code != 200 || strings.Join(attempts, ",") != "A.example,B.example" {
		t.Fatalf("%d %v %s", response.Code, attempts, response.Body.String())
	}
	response = httptest.NewRecorder()
	request = httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{"model":"public","input":"continue","previous_response_id":"resp_actual_b"}`))
	request.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(response, request)
	if response.Code != 200 || strings.Join(attempts, ",") != "A.example,B.example,B.example" {
		t.Fatalf("continuation %d %v", response.Code, attempts)
	}
	ctx := context.WithValue(context.Background(), accessTokenIDContextKey{}, contract.AccessTokenID("access_other"))
	continuation := Request{Protocol: contract.ProtocolOpenAIResponses, Model: "public", PreviousResponseID: "resp_actual_b"}
	if _, err := handler.bindResponseAffinity(ctx, continuation, candidates); err == nil {
		t.Fatal("cross-principal affinity accepted")
	}
	if _, err := handler.bindResponseAffinity(context.Background(), continuation, candidates[:1]); err == nil {
		t.Fatal("unavailable target silently switched")
	}
	continuation.PreviousResponseID = "unknown"
	if _, err := handler.bindResponseAffinity(context.Background(), continuation, candidates[:1]); err == nil {
		t.Fatal("unknown previous response accepted")
	}
}

func TestKiroContinuationDoesNotConsumeOtherProvidersResponseIDs(t *testing.T) {
	h := NewWithDependencies(Dependencies{})
	kiro := kiroSettingsCandidate("service_kiro", "")
	kiro.UpstreamProtocol = contract.ProtocolOpenAIResponses
	other := endpoint.Resolved{Service: contract.Service{ID: "service_native", Kind: contract.ServiceKindOpenAICompatible}, UpstreamProtocol: contract.ProtocolOpenAIResponses, PlanType: contract.PlanTypeNative}
	req := Request{Protocol: contract.ProtocolOpenAIResponses, Model: "m", PreviousResponseID: "resp_test"}
	candidates := []endpoint.Resolved{kiro, other}
	key := affinityKey{"", req.PreviousResponseID}
	h.affinities.entries = map[affinityKey]affinityEntry{key: {at: time.Now(), binding: storage.ResponseAffinity{ServiceID: other.Service.ID, UpstreamModel: "m", UpstreamProtocol: other.UpstreamProtocol, PlanType: contract.PlanTypeNative}}}
	selected, err := h.bindResponseAffinity(t.Context(), req, candidates)
	if err != nil || len(selected) != 1 || selected[0].Service.ID != other.Service.ID {
		t.Fatalf("lost native affinity: %v, %v", selected, err)
	}
	entry := h.affinities.entries[key]
	entry.binding.ServiceID = kiro.Service.ID
	entry.binding.PlanType = contract.PlanTypeAdapter
	h.affinities.entries[key] = entry
	_, err = h.bindResponseAffinity(t.Context(), req, candidates)
	var param *providerapi.KiroParameterError
	if !errors.As(err, &param) || param.Field != "previous_response_id" {
		t.Fatalf("wrong Kiro continuation error: %v", err)
	}
}
