package endpoint

import (
	"github.com/QuantumNous/astrlink/core/contract"
	"testing"
)

func TestKiroResolverKeepsAdapterModeAndProcessingFormat(t *testing.T) {
	service := contract.Service{ID: "service_kiro", Name: "Kiro", Kind: contract.ServiceKindKiroSubscription, Enabled: true, Models: []string{"claude-test"}, Capabilities: contract.SubscriptionProviderKiro.Capabilities(), Subscription: &contract.SubscriptionConnection{Provider: contract.SubscriptionProviderKiro, Status: contract.SubscriptionStatusConnected, CredentialRef: "local://subscription/service_kiro"}}
	service.Capabilities = append(service.Capabilities, contract.Capability{Protocol: contract.ProtocolOpenAIChat, Mode: contract.CapabilityModeAdapter, Streaming: true, ConvertTo: contract.ProtocolAnthropicMessages})
	runtime := contract.RuntimeProfile{RelayKitAvailable: true, Edges: []contract.ConversionEdge{{From: contract.ProtocolOpenAIChat, To: contract.ProtocolAnthropicMessages, Quality: contract.ConversionQualityFair, Streaming: true}}}
	for _, protocol := range []contract.ProtocolID{contract.ProtocolAnthropicMessages, contract.ProtocolOpenAIChat} {
		candidate, skip := planService(service, ResolveRequest{Protocol: protocol, Model: "claude-test", Streaming: true}, "", runtime)
		if skip != "" || candidate.Mode != contract.CapabilityModeAdapter || candidate.PlanType != contract.PlanTypeAdapter || candidate.UpstreamProtocol != contract.ProtocolAnthropicMessages {
			t.Fatalf("resolved Kiro as passthrough: %+v %s", candidate, skip)
		}
	}
	if _, skip := planService(service, ResolveRequest{Protocol: contract.ProtocolOpenAIChat, Model: "claude-test", Streaming: true}, "", contract.RuntimeProfile{}); skip != contract.RoutingSkipConversionUnavailable {
		t.Fatalf("missing intermediate converter: %s", skip)
	}
}
