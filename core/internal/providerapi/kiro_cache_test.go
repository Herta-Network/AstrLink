package providerapi

import (
	"encoding/json"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/MoeForge/moego/ojson"
	"github.com/QuantumNous/astrlink/core/contract"
)

func encodeCacheRequest(t *testing.T, body string, rt KiroRuntime) (ojson.Result, []contract.ConversionDiagnostic, error) {
	t.Helper()
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body))
	ex, err := KiroRequest(req, "profile", "scope", rt)
	if err != nil {
		return ojson.Result{}, nil, err
	}
	raw, err := io.ReadAll(req.Body)
	_ = req.Body.Close()
	return ojson.ParseBytes(raw), ex.Diagnostics(contract.ConversionDiagnosticPhaseRequest), err
}

func TestKiroGARCachesTheDeclaredBoundaries(t *testing.T) {
	body := `{
 "model":"m",
 "tools":[{"name":"read","input_schema":{"type":"object","properties":{"cache_control":{"type":"string"}}},"cache_control":{"type":"ephemeral"}},{"name":"write","input_schema":{"type":"object"}}],
 "system":[{"type":"text","text":"instructions","cache_control":{"type":"ephemeral"}}],
 "messages":[{"role":"user","content":"earlier"},
 {"role":"assistant","content":[{"type":"text","text":"answer","cache_control":{"type":"ephemeral"}}]},
 {"role":"user","content":[{"type":"text","text":"now","cache_control":{"type":"ephemeral"}}]}],
 "cache_control":{"type":"ephemeral"}
 }`
	max := 4
	rt := KiroRuntime{PromptCaching: KiroPromptCaching{SupportsPromptCaching: true, MaximumCacheCheckpointsPerRequest: &max}}
	r, lost, err := encodeCacheRequest(t, body, rt)
	if err != nil || len(lost) > 0 {
		t.Fatalf("encode %v losses %v", err, lost)
	}
	state := r.Get("conversationState")
	tools := state.Get("currentMessage.userInputMessage.userInputMessageContext.tools")
	if n := len(tools.Array()); n != 3 {
		t.Fatalf("%d tool entries: %s", n, tools.Raw)
	}
	if tools.Get("0.toolSpecification.name").String() != "read" || tools.Get("0.toolSpecification.inputSchema.json.properties.cache_control.type").String() != "string" || tools.Get("2.toolSpecification.name").String() != "write" {
		t.Fatal("tool schema or order changed")
	}
	if tools.Get("1").Raw != `{"cachePoint":{"type":"default"}}` {
		t.Fatalf("checkpoint is not a standalone Tool union member: %s", tools.Raw)
	}
	for _, path := range []string{"history.0.userInputMessage", "history.3.assistantResponseMessage", "currentMessage.userInputMessage"} {
		if got := state.Get(path + ".cachePoint").Raw; got != `{"type":"default"}` {
			t.Fatalf("checkpoint at %s = %q", path, got)
		}
	}
	if got := strings.Count(r.Raw, `"cachePoint"`); got != 4 {
		t.Fatalf("got %d checkpoints", got)
	}
	max = 3
	if _, _, err := encodeCacheRequest(t, body, rt); err == nil {
		t.Fatal("accepted too many checkpoints")
	}
	max = 0
	if _, _, err := encodeCacheRequest(t, body, rt); err == nil {
		t.Fatal("ignored a zero limit advertised by this model")
	}
}

func TestKiroCacheHintsNeverWidenBoundariesOrRetention(t *testing.T) {
	const input = `{
 "model":"m","prompt_cache_key":"local-only-key","prompt_cache_retention":"24h",
 "system":[{"type":"text","text":"fixed","cache_control":{"type":"ephemeral"}},{"type":"text","text":"variable"}],
 "messages":[{"role":"user","content":[{"type":"text","text":"early","cache_control":{"type":"ephemeral"}}]},
 {"role":"user","content":[{"type":"text","text":"same turn"}]},
 {"role":"assistant","content":"answer"},
 {"role":"user","content":[{"type":"text","text":"now","cache_control":{"type":"ephemeral","ttl":"1h"}}]}]
 }`
	r, lost, err := encodeCacheRequest(t, input, KiroRuntime{PromptCaching: KiroPromptCaching{SupportsPromptCaching: true}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(r.Raw, "cachePoint") || strings.Contains(r.Raw, "local-only-key") || strings.Contains(r.Raw, "24h") {
		t.Fatalf("unmapped values forwarded: %s", r.Raw)
	}
	if !strings.Contains(r.Get("conversationState.history.2.userInputMessage.content").String(), "same turn") {
		t.Fatal("merged content lost")
	}
	paths := map[string]bool{}
	for _, d := range lost {
		paths[d.Path] = true
	}
	for _, p := range []string{"prompt_cache_key", "prompt_cache_retention", "system.0.cache_control", "messages.0.content.0.cache_control", "messages.3.content.0.cache_control"} {
		if !paths[p] {
			t.Errorf("missing diagnostic for %s: %v", p, paths)
		}
	}
}

func TestKiroCheckpointsRespectEndpointAndSystemField(t *testing.T) {
	body := `{"model":"m","system":[{"type":"text","text":"caller","cache_control":{"type":"ephemeral"}}],"messages":[{"role":"user","content":"go"}],"cache_control":{"type":"ephemeral"}}`
	for _, tt := range []struct {
		name           string
		runtime        KiroRuntime
		points, losses int
	}{
		{"model without caching", KiroRuntime{}, 0, 2},
		{"GAR dedicated prompt", KiroRuntime{SystemField: true, PromptCaching: KiroPromptCaching{SupportsPromptCaching: true}}, 1, 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r, loss, err := encodeCacheRequest(t, body, tt.runtime)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Count(r.Raw, `"cachePoint"`) != tt.points || len(loss) != tt.losses || strings.Count(r.Raw, "caller") != 1 {
				t.Fatalf("wire=%s losses=%v", r.Raw, loss)
			}
		})
	}
}

func TestKiroCacheDoesNotInspectToolPayloadOrKeepFilteredTools(t *testing.T) {
	body := `{"model":"m","tools":[{"name":"skip","cache_control":{"type":"ephemeral"},"input_schema":{"type":"object"}},{"name":"keep","input_schema":{"type":"object","properties":{"cache_control":{"type":"object"}}},"cache_control":{"type":"ephemeral"}}],"tool_choice":{"type":"tool","name":"keep"},"messages":[{"role":"user","content":"go"},{"role":"assistant","content":[{"type":"tool_use","id":"call","name":"keep","input":{"cache_control":{"type":"ephemeral"}}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"call","content":"ok","cache_control":{"type":"ephemeral"}}]}]}`
	r, loss, err := encodeCacheRequest(t, body, KiroRuntime{PromptCaching: KiroPromptCaching{SupportsPromptCaching: true}})
	if err != nil {
		t.Fatal(err)
	}
	if len(loss) != 2 || strings.Count(r.Raw, `"cachePoint"`) != 1 {
		t.Fatalf("wire=%s loss=%v", r.Raw, loss)
	}
	payload := r.Get("conversationState.history.1.assistantResponseMessage.toolUses.0.input")
	if !json.Valid([]byte(payload.Raw)) || payload.Get("cache_control.type").String() != "ephemeral" {
		t.Fatalf("corrupted tool arguments: %s", payload.Raw)
	}
}

func TestKiroNestedToolResultCacheBoundary(t *testing.T) {
	const body = `{
 "model":"m","tools":[{"name":"read","input_schema":{"type":"object"}}],"messages":[
 {"role":"user","content":"read"},
 {"role":"assistant","content":[{"type":"tool_use","id":"call","name":"read","input":{}}]},
 {"role":"user","content":[{"type":"tool_result","tool_use_id":"call","content":[
 {"type":"text","text":"first","cache_control":{"type":"ephemeral"}},
 {"type":"text","text":"second","cache_control":{"type":"ephemeral"}}]}]}]}`
	rt := KiroRuntime{PromptCaching: KiroPromptCaching{SupportsPromptCaching: true}}
	wire, losses, err := encodeCacheRequest(t, body, rt)
	if err != nil {
		t.Fatal(err)
	}
	u := wire.Get("conversationState.currentMessage.userInputMessage")
	if u.Get("cachePoint.type").String() != "default" || u.Get("userInputMessageContext.toolResults.0.content.#").Int() != 2 {
		t.Fatalf("incorrect result boundary: %s", u.Raw)
	}
	if len(losses) != 1 || losses[0].Path != "messages.2.content.0.content.0.cache_control" {
		t.Fatalf("mid-result checkpoint moved: %+v", losses)
	}
}
