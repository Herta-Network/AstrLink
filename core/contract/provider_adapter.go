package contract

import "fmt"

// ProcessingProtocol is the standard format the gateway's policy, conversion
// and response writers handle. UpstreamProtocol remains the actual destination.
func (plan ExecutionPlan) ProcessingProtocol() ProtocolID {
	if plan.Type == PlanTypeAdapter {
		return plan.AdapterInputProtocol
	}
	return plan.UpstreamProtocol
}

func (plan ExecutionPlan) UsesRelayKit() bool {
	return plan.Type == PlanTypeRelayKit || plan.Type == PlanTypeAdapter && len(plan.ConversionPath) > 0
}

// AdaptKiroPlan adds the provider boundary after any standard-format conversion.
// The conversion path contains only RelayKit edges; the final adapter stage is
// explicit in AdapterInputProtocol -> UpstreamProtocol.
func AdaptKiroPlan(plan ExecutionPlan) ExecutionPlan {
	plan.Type = PlanTypeAdapter
	plan.AdapterInputProtocol = plan.UpstreamProtocol
	if plan.AdapterInputProtocol.IsModelDiscovery() {
		plan.UpstreamProtocol = ProtocolLocalModels
	} else {
		plan.UpstreamProtocol = ProtocolKiroAssistant
	}
	return plan
}

func (plan ExecutionPlan) validateAdapter() error {
	if plan.UpstreamProtocol == ProtocolLocalModels {
		if plan.InputProtocol != ProtocolOpenAIModels || plan.AdapterInputProtocol != ProtocolOpenAIModels || plan.Streaming || len(plan.ConversionPath) > 0 || plan.ConversionQuality != "" {
			return fmt.Errorf("invalid local model catalog adapter plan")
		}
		return nil
	}
	if plan.UpstreamProtocol != ProtocolKiroAssistant || plan.AdapterInputProtocol != ProtocolAnthropicMessages {
		return fmt.Errorf("unsupported provider adapter boundary")
	}
	if len(plan.ConversionPath) == 0 {
		if plan.InputProtocol != plan.AdapterInputProtocol || plan.ConversionQuality != "" {
			return fmt.Errorf("adapter requires a conversion path to its input format")
		}
		return nil
	}
	if !plan.ConversionQuality.Valid() {
		return fmt.Errorf("adapter conversion requires a valid quality")
	}
	current := plan.InputProtocol
	for index, edge := range plan.ConversionPath {
		if err := edge.Validate(); err != nil {
			return err
		}
		if edge.From != current || plan.Streaming && !edge.Streaming {
			return fmt.Errorf("invalid adapter conversion_path[%d]", index)
		}
		current = edge.To
	}
	if current != plan.AdapterInputProtocol {
		return fmt.Errorf("conversion must end at the adapter input format")
	}
	return nil
}
