package contract

import "testing"

func TestKiroCapabilitiesAndAuthorizationBoundary(t *testing.T) {
	provider := SubscriptionProviderKiro
	if !provider.Valid() || provider.ServiceKind() != ServiceKindKiroSubscription || !ServiceKindKiroSubscription.IsSubscription() {
		t.Fatal("Kiro subscription not registered")
	}
	capabilities := provider.Capabilities()
	for _, protocol := range []ProtocolID{ProtocolOpenAIChat, ProtocolOpenAIResponses} {
		capabilities = append(capabilities, Capability{Protocol: protocol, Mode: CapabilityModeAdapter, Streaming: true, ConvertTo: ProtocolAnthropicMessages})
	}
	if err := provider.ValidateCapabilities(capabilities); err != nil {
		t.Fatal(err)
	}
	for index := range capabilities {
		invalid := append([]Capability(nil), capabilities...)
		invalid[index].Mode = CapabilityModeNative
		if err := provider.ValidateCapabilities(invalid); err == nil {
			t.Fatalf("Kiro accepted native mode at capabilities[%d]", index)
		}
	}
	capabilities[len(capabilities)-1].ConvertTo = ProtocolGoogleGenerateContent
	if err := provider.ValidateCapabilities(capabilities); err == nil {
		t.Fatal("Kiro accepted a non-Messages upstream")
	}
	for _, flow := range []AuthorizationFlow{AuthorizationFlowClaudeCLI} {
		if flow.SupportedBy(provider) {
			t.Fatalf("unsupported Kiro flow %s", flow)
		}
	}
	if !AuthorizationFlowDeviceCode.SupportedBy(provider) {
		t.Fatal("missing device code flow")
	}
}
