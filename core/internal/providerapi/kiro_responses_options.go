package providerapi

import (
	"encoding/json"
	"maps"
	"slices"
	"strings"

	"github.com/MoeForge/moego/ojson"
	"github.com/QuantumNous/astrlink/core/contract"
)

// KiroContinuationError is shared with response-affinity routing: even a
// known binding identifies only the provider, not stored conversation items.
func KiroContinuationError() *KiroParameterError {
	return &KiroParameterError{Field: "previous_response_id", Reason: "Kiro does not retain responses; resend the full conversation in input without previous_response_id"}
}

// CreateResponse shares the Responses event and input formats, but accepts a
// smaller set of request options. Do not silently relax a caller's hard limits.
func kiroResponsesOptions(root ojson.Result, exchange *KiroExchange) (map[string]any, error) {
	if p := root.Get("previous_response_id"); p.Exists() && p.Type != ojson.Null {
		return nil, KiroContinuationError()
	}
	body := make(map[string]any)
	fields := root.Map()
	for _, key := range slices.Sorted(maps.Keys(fields)) {
		v := fields[key]
		if v.Type == ojson.Null {
			continue
		}
		switch key {
		case "tools":
			if !v.IsArray() {
				return nil, kiroParameter("tools", "expected a list of tool definitions")
			}
			body[key] = json.RawMessage(v.Raw)
		case "model", "instructions", "user", "safety_identifier":
			body[key] = json.RawMessage(v.Raw)
		case "input", "stream", "origin", "profileArn", "metadata", "prompt_cache_key", "prompt_cache_retention", "tool_choice", "parallel_tool_calls":
			// Input and Kiro identity are encoded separately. Tool options must
			// be resolved together, independently of the order of JSON keys.
		case "client_metadata":
			// Some Codex versions attach client telemetry outside metadata.
			// Never log its values or send it as Kiro-owned metadata.
			exchange.note(contract.ConversionDiagnosticPhaseRequest, "kiro_client_metadata_omitted", "client_metadata", "Kiro does not consume client telemetry.")
		case "reasoning":
			reasoning, err := kiroResponsesReasoning(v, exchange)
			if err != nil {
				return nil, err
			}
			if len(reasoning) > 0 {
				body[key] = reasoning
			}
		case "store", "background":
			if v.Type != ojson.False {
				return nil, kiroParameter(key, "Kiro supports only synchronous, non-persisted responses; omit this field or set it to false")
			}
			if key == "store" {
				body[key] = false
			}
		case "max_output_tokens":
			return nil, kiroParameter(key, "Kiro CreateResponse cannot enforce an output token limit")
		case "temperature", "top_p":
			if v.Type != ojson.Number || v.Num != 1 {
				return nil, kiroParameter(key, "Kiro CreateResponse has no sampling control for this field; omit it to use the model default")
			}
		case "include":
			if !v.IsArray() {
				return nil, kiroParameter(key, "expected an array of output fields")
			}
			for _, field := range v.Array() {
				if field.Type != ojson.String || field.Str != "reasoning.encrypted_content" {
					return nil, kiroParameter(key, "only reasoning.encrypted_content can be supplied by the native reasoning output")
				}
			}
		case "text":
			if err := kiroResponsesText(v, exchange); err != nil {
				return nil, err
			}
		case "service_tier":
			if v.Type != ojson.String || (v.Str != "auto" && v.Str != "default") {
				return nil, kiroParameter(key, "Kiro cannot select a service tier; omit it to use the default")
			}
		case "truncation":
			if v.Type != ojson.String || v.Str != "disabled" {
				return nil, kiroParameter(key, "Kiro cannot automatically truncate the conversation; send input that fits")
			}
		case "conversation":
			return nil, kiroParameter(key, "Kiro does not store conversations; resend the full conversation in input")
		default:
			// Unknown key names are caller-controlled; do not include them in a record.
			return nil, kiroParameter("request", "unrecognized top-level field for Kiro CreateResponse")
		}
	}

	if choice := root.Get("tool_choice"); choice.Exists() && choice.Type != ojson.Null {
		switch {
		case choice.Type == ojson.String && choice.Str == "auto":
		case choice.Type == ojson.String && choice.Str == "none":
			delete(body, "tools")
		default:
			return nil, kiroParameter("tool_choice", "Kiro CreateResponse supports auto or none; it cannot guarantee a required or named tool call")
		}
	}
	if parallel := root.Get("parallel_tool_calls"); parallel.Exists() && parallel.Type != ojson.Null {
		switch parallel.Type {
		case ojson.True:
		case ojson.False:
			if _, ok := body["tools"]; ok && len(root.Get("tools").Array()) > 0 {
				return nil, kiroParameter("parallel_tool_calls", "Kiro cannot guarantee serial tool calls when tools are enabled")
			}
		default:
			return nil, kiroParameter("parallel_tool_calls", "expected a boolean")
		}
	}
	return body, nil
}

func kiroResponsesText(text ojson.Result, exchange *KiroExchange) error {
	if !text.IsObject() {
		return kiroParameter("text", "expected a text configuration object")
	}
	for key, v := range text.Map() {
		if v.Type == ojson.Null {
			continue
		}
		switch key {
		case "format":
			if !v.IsObject() || v.Get("type").Str != "text" || len(v.Map()) != 1 {
				return kiroParameter("text.format", "Kiro CreateResponse cannot enforce a structured output format")
			}
		case "verbosity":
			if v.Type != ojson.String || (v.Str != "low" && v.Str != "medium" && v.Str != "high") {
				return kiroParameter("text.verbosity", "expected low, medium, or high")
			}
			exchange.note(contract.ConversionDiagnosticPhaseRequest, "kiro_text_verbosity_omitted", "text.verbosity", "Kiro uses the model's default verbosity.")
		default:
			return kiroParameter("text", "unsupported output text option")
		}
	}
	return nil
}

func kiroResponsesReasoning(reasoning ojson.Result, exchange *KiroExchange) (map[string]any, error) {
	if !reasoning.IsObject() {
		return nil, kiroParameter("reasoning", "expected a reasoning configuration object")
	}
	out := map[string]any{}
	for key, value := range reasoning.Map() {
		if value.Type == ojson.Null {
			continue
		}
		switch key {
		case "type", "effort":
			if value.Type != ojson.String {
				return nil, kiroParameter("reasoning."+key, "expected a string")
			}
			out[key] = value.Str
		case "context":
			// Codex sends all_turns with its full input. Kiro already receives
			// those reasoning items in order; it has no server-side history selector.
			if value.Type != ojson.String || value.Str != "all_turns" {
				return nil, kiroParameter("reasoning.context", "Kiro uses the reasoning history included in input; send the intended items explicitly")
			}
		case "summary": // handled after type, regardless of JSON key order
		default:
			return nil, kiroParameter("reasoning", "unsupported reasoning option")
		}
	}
	if summary := reasoning.Get("summary"); summary.Exists() && summary.Type != ojson.Null {
		if summary.Type != ojson.String {
			return nil, kiroParameter("reasoning.summary", "expected a string")
		}
		if strings.HasPrefix(exchange.Model, "gpt-") {
			exchange.note(contract.ConversionDiagnosticPhaseRequest, "kiro_reasoning_summary_omitted", "reasoning.summary", "Kiro GPT models do not accept a reasoning summary option.")
			return out, nil
		}
		if !strings.HasPrefix(exchange.Model, "claude-") {
			out["summary"] = summary.Str
			return out, nil
		}
		switch summary.Str {
		case "auto", "concise", "detailed":
			out["summary"] = "summarized"
			exchange.note(contract.ConversionDiagnosticPhaseRequest, "kiro_reasoning_summary_adapted", "reasoning.summary", "Kiro returns its supported summarized display.")
		case "summarized", "omitted":
			out["summary"] = summary.Str
		default:
			return nil, kiroParameter("reasoning.summary", "unsupported summary display")
		}
		if _, ok := out["type"]; !ok {
			out["type"] = "adaptive"
		}
	}
	return out, nil
}
