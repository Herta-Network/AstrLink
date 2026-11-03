package contracts_test

import "testing"

func TestProviderAdapterSchemaAndRuntimeAdvertisement(t *testing.T) {
	capabilities := loadFixture(t)
	capabilities["plan_types"] = append(capabilities["plan_types"].([]any), map[string]any{"id": "adapter", "available_in_alpha": false, "uses_local_conversion": true})
	if err := compileSchema(t, capabilitySchemaURL).Validate(capabilities); err != nil {
		t.Fatal(err)
	}
	plan := map[string]any{"plan_type": "adapter", "service_id": "service_kiro", "input_protocol": "anthropic.messages", "adapter_input_protocol": "anthropic.messages", "upstream_protocol": "kiro.generate_assistant_response", "streaming": true, "conversion_path": []any{}}
	schema := compileSchema(t, capabilitySchemaURL+"#/$defs/ExecutionPlan")
	if err := schema.Validate(plan); err != nil {
		t.Fatal(err)
	}
	delete(plan, "adapter_input_protocol")
	if schema.Validate(plan) == nil {
		t.Fatal("adapter input omitted without rejection")
	}
	plan["adapter_input_protocol"] = "anthropic.messages"
	plan["plan_type"] = "native"
	if schema.Validate(plan) == nil {
		t.Fatal("adapter advertised as a native plan")
	}
}
