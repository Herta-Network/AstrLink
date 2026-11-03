package planner

import (
	"fmt"

	"github.com/QuantumNous/astrlink/core/contract"
)

// BuildDirect selects a directly accepted ingress format. Unlike BuildAlpha,
// it can also select a built-in provider adapter, which does not preserve wire.
func BuildDirect(input AlphaInput) (contract.ExecutionPlan, error) {
	service := input.Service
	if service.ID == "" && input.Endpoint.ID != "" {
		service = contract.ServiceFromEndpoint(input.Endpoint)
	}
	if service.Kind != contract.ServiceKindKiroSubscription {
		return BuildAlpha(input)
	}
	if input.Mode != contract.CapabilityModeAdapter {
		return contract.ExecutionPlan{}, ErrCapabilityUnavailable
	}
	if err := service.Validate(); err != nil {
		return contract.ExecutionPlan{}, err
	}
	if !service.Enabled {
		return contract.ExecutionPlan{}, ErrEndpointDisabled
	}
	found := false
	for _, capability := range service.Capabilities {
		if capability.Protocol == input.Protocol && capability.Mode == contract.CapabilityModeAdapter && capability.ConvertTo == "" && (!input.Streaming || capability.Streaming) {
			found = true
			break
		}
	}
	if !found {
		return contract.ExecutionPlan{}, &CapabilityUnavailableError{Protocol: input.Protocol, Mode: contract.CapabilityModeAdapter, Streaming: input.Streaming}
	}
	plan := contract.AdaptKiroPlan(contract.ExecutionPlan{ServiceID: service.ID, InputProtocol: input.Protocol, UpstreamProtocol: input.Protocol, ConversionPath: []contract.ConversionEdge{}, Streaming: input.Streaming})
	if err := plan.ValidateForRuntime(contract.RuntimeProfile{}); err != nil {
		return contract.ExecutionPlan{}, fmt.Errorf("provider adapter plan: %w", err)
	}
	return plan, nil
}
