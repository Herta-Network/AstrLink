package providerapi

import (
	"encoding/json"
	"fmt"
	"math"

	"github.com/MoeForge/moego/ojson"
	"github.com/QuantumNous/astrlink/core/contract"
)

// KiroRuntime describes one account and model for a single network attempt.
// The prompt-field assignment is shared; the model schema and cache checkpoints apply to GAR.
type KiroRuntime struct {
	ChatCompletions bool
	MaxOutputTokens int // Catalog ceiling, including models with no per-request output control.
	PromptCaching   KiroPromptCaching
	replay          kiroReplayScope
	Profile         string
	SystemField     bool
	ModelSchema     json.RawMessage
}

func (runtime KiroRuntime) modelFields(root ojson.Result, exchange *KiroExchange) (map[string]any, error) {
	props := ojson.GetBytes(runtime.ModelSchema, "properties")
	fields := map[string]any{}
	for _, key := range []string{"temperature", "top_p", "max_tokens"} {
		v, p := root.Get(key), props.Get(key)
		if !v.Exists() {
			continue
		}
		if !p.IsObject() {
			if key == "max_tokens" {
				if !isPositiveTokenCount(v) {
					return nil, kiroParameter(key, "expected a positive integer")
				}
				if runtime.MaxOutputTokens > 0 {
					if v.Num >= float64(runtime.MaxOutputTokens) {
						// The model itself cannot exceed the caller's limit.
						continue
					}
					return nil, kiroParameter(key, fmt.Sprintf("the Kiro model's fixed output ceiling is %d; it cannot enforce a smaller limit", runtime.MaxOutputTokens))
				}
				return nil, kiroParameter(key, "this Kiro model does not advertise an enforceable output limit")
			}
			return nil, kiroParameter(key, "this Kiro model does not advertise a control for this parameter")
		}
		if key == "max_tokens" {
			if !isPositiveTokenCount(v) {
				return nil, kiroParameter(key, "expected a positive integer")
			}
			if minimum := p.Get("minimum"); minimum.Type == ojson.Number && v.Num < minimum.Num {
				return nil, kiroParameter(key, fmt.Sprintf("the Kiro model requires at least %g; the requested output limit cannot be honored", minimum.Num))
			}
			maximum := p.Get("maximum")
			if isPositiveTokenCount(maximum) && v.Num > maximum.Num {
				exchange.note(contract.ConversionDiagnosticPhaseRequest, "kiro_max_tokens_clamped", key,
					fmt.Sprintf("Reduced max_tokens from %g to the Kiro model maximum of %g.", v.Num, maximum.Num))
				v = maximum
			}
		}
		if v.Type != ojson.Number || !kiroSchemaAllows(p, v) {
			return nil, kiroParameter(key, "value is outside this Kiro model's schema")
		}
		fields[key] = json.RawMessage(v.Raw)
	}
	if v := root.Get("thinking"); v.Exists() && v.Type != ojson.Null {
		out, err := kiroThinkingFields(v, props.Get("thinking.properties"), exchange)
		if err != nil {
			return nil, err
		}
		if len(out) > 0 {
			fields["thinking"] = out
		}
	}
	if cfg := root.Get("output_config"); cfg.Exists() && cfg.Type != ojson.Null {
		if !cfg.IsObject() {
			return nil, kiroParameter("output_config", "expected an object")
		}
		for key := range cfg.Map() {
			if key != "effort" {
				return nil, kiroParameter("output_config", "only effort can be represented by this Kiro model")
			}
		}
	}
	if v := root.Get("output_config.effort"); v.Exists() {
		target := "output_config"
		schema := props.Get(target + ".properties.effort")
		if !schema.IsObject() {
			target = "reasoning"
			schema = props.Get(target + ".properties.effort")
		}
		if !schema.IsObject() || !kiroSchemaAllows(schema, v) {
			return nil, kiroParameter("output_config.effort", "the Kiro model does not support this reasoning effort")
		}
		fields[target] = map[string]any{"effort": v.String()}
	}
	return fields, nil
}

// Prefer an exact native representation. Adaptive-only models can accept a
// request for thinking, but have no numeric thinking-budget control.
func kiroThinkingFields(v, props ojson.Result, exchange *KiroExchange) (map[string]any, error) {
	kind := v.Get("type")
	if !v.IsObject() || kind.Type != ojson.String {
		return nil, kiroParameter("thinking.type", "expected a thinking mode")
	}
	for key := range v.Map() {
		switch key {
		case "type", "budget_tokens", "display":
		default:
			return nil, kiroParameter("thinking", "unrecognized thinking option")
		}
	}
	budget := v.Get("budget_tokens")
	hasBudget := budget.Exists() && budget.Type != ojson.Null
	if hasBudget && (kind.Str != "enabled" || !isPositiveTokenCount(budget)) {
		return nil, kiroParameter("thinking.budget_tokens", "expected a positive integer with enabled thinking")
	}
	types := props.Get("type")
	if exchange.Model == "claude-sonnet-5.5" && kind.Str == "between_tools" {
		return nil, kiroParameter("thinking.type", "Kiro rejects between_tools for Sonnet 5.5; use adaptive")
	}
	native := types.IsObject() && kiroSchemaAllows(types, kind)
	if !native && kind.Str == "disabled" {
		exchange.note(contract.ConversionDiagnosticPhaseRequest, "kiro_thinking_disabled_omitted", "thinking.type", "Kiro has no explicit disabled setting for this model; its default applies.")
		return nil, nil
	}
	if kind.Str == "enabled" && (!native || hasBudget && !props.Get("budget_tokens").IsObject()) && types.IsObject() && kiroSchemaAllows(types, ojson.Parse(`"adaptive"`)) {
		kind = ojson.Parse(`"adaptive"`)
		exchange.note(contract.ConversionDiagnosticPhaseRequest, "kiro_thinking_adapted", "thinking.type", "Kiro uses adaptive thinking for this model.")
		if hasBudget {
			hasBudget = false
			exchange.note(contract.ConversionDiagnosticPhaseRequest, "kiro_thinking_budget_omitted", "thinking.budget_tokens", "The model chooses its own thinking budget in adaptive mode.")
		}
	} else if !native {
		return nil, kiroParameter("thinking.type", "unsupported thinking mode for this Kiro model")
	}
	out := map[string]any{"type": kind.Str}
	if hasBudget {
		p := props.Get("budget_tokens")
		if !p.IsObject() || !kiroSchemaAllows(p, budget) {
			return nil, kiroParameter("thinking.budget_tokens", "unsupported budget for this Kiro model")
		}
		out["budget_tokens"] = json.RawMessage(budget.Raw)
	}
	if display := v.Get("display"); display.Exists() && display.Type != ojson.Null {
		schema := props.Get("display")
		if !schema.IsObject() || !kiroSchemaAllows(schema, display) {
			summary := ojson.Parse(`"summarized"`)
			if display.Str != "updates" || !schema.IsObject() || !kiroSchemaAllows(schema, summary) {
				return nil, kiroParameter("thinking.display", "unsupported display for this Kiro model")
			}
			display = summary
			exchange.note(contract.ConversionDiagnosticPhaseRequest, "kiro_thinking_display_adapted", "thinking.display", "Kiro returns summarized thinking instead of progress updates.")
		}
		out["display"] = json.RawMessage(display.Raw)
	}
	return out, nil
}

func isPositiveTokenCount(value ojson.Result) bool {
	return value.Type == ojson.Number && value.Num > 0 && !math.IsInf(value.Num, 0) && math.Trunc(value.Num) == value.Num
}

func kiroSchemaAllows(schema, value ojson.Result) bool {
	switch schema.Get("type").String() {
	case "integer", "number":
		if value.Type != ojson.Number || schema.Get("type").String() == "integer" && value.Float() != float64(value.Int()) {
			return false
		}
		if min := schema.Get("minimum"); min.Exists() && value.Float() < min.Float() {
			return false
		}
		if max := schema.Get("maximum"); max.Exists() && value.Float() > max.Float() {
			return false
		}
	case "string":
		if value.Type != ojson.String {
			return false
		}
	case "": // Some catalogs describe a string using only its enum.
		if !schema.Get("enum").IsArray() {
			return false
		}
	default:
		return false
	}
	if values := schema.Get("enum"); values.IsArray() {
		for _, v := range values.Array() {
			if v.Type == value.Type && v.String() == value.String() {
				return true
			}
		}
		return false
	}
	return true
}
