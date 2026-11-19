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

func TestKiroCreateResponsePlanAcceptsDirectResponsesAndConvertedChat(t *testing.T) {
	schema := compileSchema(t, capabilitySchemaURL+"#/$defs/ExecutionPlan")
	plan := map[string]any{"plan_type": "adapter", "service_id": "service_kiro", "input_protocol": "openai.responses", "adapter_input_protocol": "openai.responses", "upstream_protocol": "kiro.create_response", "streaming": true, "conversion_path": []any{}}
	if err := schema.Validate(plan); err != nil {
		t.Fatal(err)
	}
	plan["adapter_input_protocol"] = "anthropic.messages"
	if err := schema.Validate(plan); err == nil {
		t.Fatal("CreateResponse accepted a Messages intermediate")
	}
	plan["adapter_input_protocol"] = "openai.responses"
	plan["input_protocol"] = "openai.chat"
	if schema.Validate(plan) == nil {
		t.Fatal("Chat accepted without conversion")
	}
	edge := map[string]any{"from": "openai.chat", "to": "openai.responses", "quality": "fair", "streaming": true}
	plan["conversion_path"], plan["conversion_quality"] = []any{edge}, "fair"
	if err := schema.Validate(plan); err != nil {
		t.Fatal(err)
	}
	edge["to"] = "anthropic.messages"
	if schema.Validate(plan) == nil {
		t.Fatal("CreateResponse accepted a Chat to Messages edge")
	}

}
