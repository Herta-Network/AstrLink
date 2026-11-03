package planner

import (
	"encoding/json"
	"testing"

	"github.com/QuantumNous/astrlink/core/contract"
)

func kiroService() contract.Service {
	return contract.Service{ID: "service_kiro", Name: "Kiro", Kind: contract.ServiceKindKiroSubscription, Enabled: true, Models: []string{"claude-test"}, Capabilities: contract.SubscriptionProviderKiro.Capabilities(), Subscription: &contract.SubscriptionConnection{Provider: contract.SubscriptionProviderKiro, Status: contract.SubscriptionStatusDisconnected}}
}

func TestKiroPlanReportsTheActualWireAndIntermediateFormat(t *testing.T) {
	service := kiroService()
	if _, err := BuildDirect(AlphaInput{Service: service, Protocol: contract.ProtocolAnthropicMessages, Mode: contract.CapabilityModeNative}); err == nil {
		t.Fatal("Kiro accepted native forwarding mode")
	}
	for _, protocol := range []contract.ProtocolID{contract.ProtocolAnthropicMessages, contract.ProtocolOpenAIChat, contract.ProtocolOpenAIResponses} {
		var plan contract.ExecutionPlan
		var err error
		converted := protocol != contract.ProtocolAnthropicMessages
		if !converted {
			plan, err = BuildDirect(AlphaInput{Service: service, Protocol: protocol, Mode: contract.CapabilityModeAdapter, Streaming: true})
		} else {
			service.Capabilities = append(service.Capabilities, contract.Capability{Protocol: protocol, Mode: contract.CapabilityModeAdapter, Streaming: true, ConvertTo: contract.ProtocolAnthropicMessages})
			plan, err = BuildRelayKit(RelayKitInput{Service: service, InputProtocol: protocol, UpstreamProtocol: contract.ProtocolAnthropicMessages, Streaming: true, Edges: []contract.ConversionEdge{{From: protocol, To: contract.ProtocolAnthropicMessages, Quality: contract.ConversionQualityFair, Streaming: true}}})
		}
		if err != nil {
			t.Fatal(err)
		}
		if plan.Type != contract.PlanTypeAdapter || plan.UpstreamProtocol != contract.ProtocolKiroAssistant || plan.AdapterInputProtocol != contract.ProtocolAnthropicMessages || plan.UsesRelayKit() != converted {
			t.Fatalf("mislabelled Kiro plan: %+v", plan)
		}
		if err := plan.ValidateForAlpha(); err == nil {
			t.Fatal("provider adapter was advertised as frozen Alpha passthrough")
		}
		raw, _ := json.Marshal(plan)
		var decoded contract.ExecutionPlan
		if err := json.Unmarshal(raw, &decoded); err != nil || decoded.Validate() != nil || decoded.UpstreamProtocol != contract.ProtocolKiroAssistant {
			t.Fatalf("plan round trip: %s %v", raw, err)
		}
		if converted {
			if err := plan.ValidateForRuntime(contract.RuntimeProfile{}); err == nil {
				t.Fatal("missing RelayKit runtime accepted")
			}
		}
		bad := plan
		bad.AdapterInputProtocol = contract.ProtocolOpenAIChat
		if bad.Validate() == nil {
			t.Fatal("unsupported adapter input accepted")
		}
	}
}

func TestKiroCatalogIsLocalAndNotAnUpstreamModelsProtocol(t *testing.T) {
	plan, err := BuildDirect(AlphaInput{Service: kiroService(), Protocol: contract.ProtocolOpenAIModels, Mode: contract.CapabilityModeAdapter})
	if err != nil || plan.Type != contract.PlanTypeAdapter || plan.UpstreamProtocol != contract.ProtocolLocalModels || plan.UsesRelayKit() {
		t.Fatalf("catalog plan: %+v %v", plan, err)
	}
	if _, err := BuildDirect(AlphaInput{Service: kiroService(), Protocol: contract.ProtocolKiroAssistant, Mode: contract.CapabilityModeAdapter}); err == nil {
		t.Fatal("provider-only wire exposed as a client ingress")
	}
}
