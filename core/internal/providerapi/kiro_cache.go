package providerapi

import (
	"fmt"
	"slices"

	"github.com/MoeForge/moego/ojson"
	"github.com/QuantumNous/astrlink/core/contract"
)

// KiroPromptCaching is the model catalog's promptCaching descriptor. Only the
// provider's tokenizer can decide whether a prefix meets the minimum length.
type KiroPromptCaching struct {
	SupportsPromptCaching             bool `json:"supportsPromptCaching"`
	MaximumCacheCheckpointsPerRequest *int `json:"maximumCacheCheckpointsPerRequest"`
	MinimumTokensPerCacheCheckpoint   *int `json:"minimumTokensPerCacheCheckpoint"`
}

// CLI 3.4.0's arm64 GAR encoder writes {"type":"default"} on an assistant
// turn (0x100820714), user turn (0x100821250), or as a Tool union member
// (0x10083f314). The IDE's CreateResponse input builder has no such item.
type kiroCachePoint struct {
	Type string `json:"type"`
}

func (e *KiroExchange) note(phase contract.ConversionDiagnosticPhase, code, path, message string) {
	entry := contract.ConversionDiagnostic{Phase: phase, Severity: contract.ConversionDiagnosticWarning, Code: code, Path: path, Message: message}
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.diagnostics) < contract.MaxConversionDiagnostics && !slices.Contains(e.diagnostics, entry) {
		e.diagnostics = append(e.diagnostics, entry)
	}
}

// RecordResponseLoss accepts only adapter-owned field paths and messages.
func (e *KiroExchange) RecordResponseLoss(code, path, message string) {
	e.note(contract.ConversionDiagnosticPhaseResponse, code, path, message)
}

func (e *KiroExchange) Diagnostics(phase contract.ConversionDiagnosticPhase) []contract.ConversionDiagnostic {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []contract.ConversionDiagnostic
	for _, d := range e.diagnostics {
		if d.Phase == phase {
			out = append(out, d)
		}
	}
	return out
}

type kiroCheckpoints struct {
	runtime  KiroRuntime
	exchange *KiroExchange
	count    int
}

// apply marks one representable boundary. Unsupported hints are optional;
// record their loss without changing the prompt or extending its retention.
func (c *kiroCheckpoints) apply(control ojson.Result, path string, slot **kiroCachePoint) {
	if !control.Exists() || control.Type == ojson.Null {
		return
	}
	reason := ""
	switch {
	case !c.runtime.PromptCaching.SupportsPromptCaching:
		reason = "The selected Kiro model does not advertise prompt caching."
	case !control.IsObject() || control.Get("type").String() != "ephemeral" || len(control.Map()) != 1:
		reason = "Kiro's default checkpoint cannot represent this cache type, explicit TTL or scope."
	case slot == nil:
		reason = "This cache boundary does not coincide with a Kiro message or tool boundary."
	}
	if reason != "" {
		c.exchange.note(contract.ConversionDiagnosticPhaseRequest, "kiro_cache_control_omitted", path, reason)
		return
	}
	if *slot == nil {
		*slot = &kiroCachePoint{Type: "default"}
		c.count++
	}
}

func (c *kiroCheckpoints) blocks(content ojson.Result, path string, slot **kiroCachePoint) {
	blocks := content.Array()
	for i, b := range blocks {
		var end **kiroCachePoint
		if i == len(blocks)-1 {
			end = slot
		}
		if control := b.Get("cache_control"); control.Exists() {
			c.apply(control, fmt.Sprintf("%s.%d.cache_control", path, i), end)
		}
		// The last part of the last tool result can coincide with the end
		// of this turn. Never traverse caller-owned tool arguments or schemas.
		if b.Get("type").String() == "tool_result" && b.Get("content").IsArray() {
			parts := b.Get("content").Array()
			for j, part := range parts {
				if control := part.Get("cache_control"); control.Exists() {
					var resultEnd **kiroCachePoint
					if j == len(parts)-1 {
						resultEnd = end
					}
					c.apply(control, fmt.Sprintf("%s.%d.content.%d.cache_control", path, i, j), resultEnd)
				}
			}
		}
	}
}

func (c *kiroCheckpoints) finish(root ojson.Result, current *kiroUser) error {
	for _, d := range kiroCacheKeys(root) {
		c.exchange.note(d.Phase, d.Code, d.Path, d.Message)
	}
	// A top-level automatic breakpoint falls on the last user turn. An
	// existing breakpoint at the same position consumes only one slot.
	c.apply(root.Get("cache_control"), "cache_control", &current.CachePoint)
	if maximum := c.runtime.PromptCaching.MaximumCacheCheckpointsPerRequest; maximum != nil && c.count > *maximum {
		return fmt.Errorf("Kiro model permits %d cache checkpoints, got %d", *maximum, c.count)
	}
	return nil
}

func (c *kiroCheckpoints) message(blocks ojson.Result, index int, turn kiroTurn, wholeTurn bool) {
	if !blocks.IsArray() {
		return
	}
	var slot **kiroCachePoint
	if wholeTurn {
		if turn.User != nil {
			slot = &turn.User.CachePoint
		} else {
			slot = &turn.Assistant.CachePoint
		}
	}
	c.blocks(blocks, fmt.Sprintf("messages.%d.content", index), slot)
}

func (c *kiroCheckpoints) tool(control ojson.Result, index int, tools *[]kiroTool) {
	if !control.Exists() || control.Type == ojson.Null {
		return
	}
	path := fmt.Sprintf("tools.%d.cache_control", index)
	if tools == nil {
		c.apply(control, path, nil)
		return
	}
	var point *kiroCachePoint
	c.apply(control, path, &point)
	if point != nil {
		*tools = append(*tools, kiroTool{CachePoint: point})
	}
}

// KiroSourceCacheDiagnostics retains cache losses across RelayKit conversion.
// Chat and Responses cache keys describe an upstream prefix and cannot serve
// as Kiro conversation IDs. Never retain or log their caller-supplied values.
func KiroSourceCacheDiagnostics(body []byte) []contract.ConversionDiagnostic {
	return kiroCacheKeys(ojson.ParseBytes(body))
}

func kiroCacheKeys(root ojson.Result) []contract.ConversionDiagnostic {
	var losses []contract.ConversionDiagnostic
	for _, key := range []string{"prompt_cache_key", "prompt_cache_retention"} {
		if v := root.Get(key); v.Exists() && v.Type != ojson.Null {
			losses = append(losses, contract.ConversionDiagnostic{
				Phase: contract.ConversionDiagnosticPhaseRequest, Severity: contract.ConversionDiagnosticWarning, Code: "kiro_cache_key_omitted", Path: key,
				Message: "The Kiro request format has no corresponding cache key or retention field.",
			})
		}
	}
	return losses
}
