package providerapi

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func TestKiroInlineDocumentFormatsPreserveBytes(t *testing.T) {
	for media, format := range kiroDocumentFormats {
		t.Run(format, func(t *testing.T) {
			bytes := []byte{0, 1, 2, 3, 255, 128, 42}
			encoded := base64.StdEncoding.EncodeToString(bytes)
			payload := map[string]any{"model": "m", "messages": []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "document", "title": "report." + format, "source": map[string]any{"type": "base64", "media_type": media, "data": encoded}}}}}}
			raw, _ := json.Marshal(payload)
			req := httptest.NewRequest("POST", "https://example.test/v1/messages", strings.NewReader(string(raw)))
			if _, err := KiroRequest(req, "", ""); err != nil {
				t.Fatal(err)
			}
			out, _ := io.ReadAll(req.Body)
			doc := gjson.GetBytes(out, "conversationState.currentMessage.userInputMessage.documents.0")
			if doc.Get("source.bytes").String() != encoded || doc.Get("format").String() != format {
				t.Fatalf("document changed: %s", out)
			}
		})
	}
}

func TestKiroDocumentsInToolResultsAndDuplicateNames(t *testing.T) {
	body := `{"model":"m","messages":[{"role":"user","content":"read"},{"role":"assistant","content":[{"type":"tool_use","id":"call_1","name":"read","input":{}}]},{"role":"user","content":[{"type":"document","title":"a.txt","source":{"type":"text","data":"first"}},{"type":"tool_result","tool_use_id":"call_1","content":[{"type":"text","text":"tool text"},{"type":"document","title":"a.txt","source":{"type":"text","data":"second"}}]}]}]}`
	req := httptest.NewRequest("POST", "https://example.test/v1/messages", strings.NewReader(body))
	if _, err := KiroRequest(req, "", ""); err != nil {
		t.Fatal(err)
	}
	out, _ := io.ReadAll(req.Body)
	current := gjson.GetBytes(out, "conversationState.currentMessage.userInputMessage")
	docs := current.Get("documents").Array()
	if len(docs) != 2 || docs[0].Get("name").String() == docs[1].Get("name").String() {
		t.Fatalf("documents lost or collide: %s", out)
	}
	for i, text := range []string{"first", "second"} {
		decoded, _ := base64.StdEncoding.DecodeString(docs[i].Get("source.bytes").String())
		if string(decoded) != text {
			t.Fatal("document bytes changed")
		}
	}
	result := current.Get("userInputMessageContext.toolResults.0")
	if result.Get("toolUseId").String() != "call_1" || !strings.Contains(result.Get("content.0.text").String(), "tool text") {
		t.Fatal("tool result association lost")
	}
}

func TestKiroRejectsInvalidDocumentsInsteadOfDroppingThem(t *testing.T) {
	for _, block := range []string{
		`{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"bad!!!"}}`,
		`{"type":"document","source":{"type":"base64","media_type":"application/x-unknown","data":"aGk="}}`,
		`{"type":"input_file","file_id":"hosted-file"}`,
		`{"type":"document","source":{"type":"url","url":"https://example.test/file.pdf"}}`,
	} {
		if _, err := kiroDocumentBlock(gjson.Parse(block)); err == nil {
			t.Fatalf("unsupported document accepted: %s", block)
		}
	}
	block, _ := json.Marshal(map[string]any{"source": map[string]any{"type": "text", "data": strings.Repeat("x", kiroMaxDocumentBytes+1)}})
	if _, err := kiroDocumentBlock(gjson.ParseBytes(block)); err == nil {
		t.Fatal("oversized document accepted")
	}
}

func TestKiroForcedToolChoiceCompatibility(t *testing.T) {
	tools := `[{"name":"first","description":"first tool","input_schema":{"type":"object","properties":{"n":{"const":9007199254740993}}}},{"name":"second","description":"second tool","input_schema":{"type":"object"}}]`
	for _, test := range []struct {
		choice    string
		count     int
		directive string
	}{
		{`{"type":"auto"}`, 2, ""}, {`{"type":"none"}`, 0, ""}, {`{"type":"any"}`, 2, "Call at least one available tool."}, {`{"type":"tool","name":"second"}`, 1, `The client requires the tool "second"`},
	} {
		body := `{"model":"m","tools":` + tools + `,"tool_choice":` + test.choice + `,"messages":[{"role":"user","content":"original prompt"}]}`
		req := httptest.NewRequest("POST", "https://example.test/v1/messages", strings.NewReader(body))
		if _, err := KiroRequest(req, "", ""); err != nil {
			t.Fatal(err)
		}
		raw, _ := io.ReadAll(req.Body)
		current := gjson.GetBytes(raw, "conversationState.currentMessage.userInputMessage")
		if len(current.Get("userInputMessageContext.tools").Array()) != test.count || !strings.Contains(current.Get("content").String(), "original prompt") || !strings.Contains(current.Get("content").String(), test.directive) {
			t.Fatalf("wrong tool choice compatibility: %s", raw)
		}
		if test.count == 1 && current.Get("userInputMessageContext.tools.0.toolSpecification.name").String() != "second" {
			t.Fatal("forced tool was not selected")
		}
		if test.count == 2 && !strings.Contains(string(raw), "9007199254740993") {
			t.Fatal("tool schema precision lost")
		}
	}
	for _, choice := range []string{`{"type":"tool","name":"missing"}`, `{"type":"tool"}`, `{"type":"invalid"}`} {
		req := httptest.NewRequest("POST", "https://example.test/v1/messages", strings.NewReader(`{"model":"m","tools":`+tools+`,"tool_choice":`+choice+`,"messages":[{"role":"user","content":"hello"}]}`))
		if _, err := KiroRequest(req, "", ""); err == nil {
			t.Fatalf("invalid choice accepted: %s", choice)
		}
	}
}

func TestKiroDocumentConversationAnchorSurvivesLaterTurns(t *testing.T) {
	id := func(text string, followup bool) string {
		first := map[string]any{"role": "user", "content": []any{map[string]any{"type": "document", "title": "file.txt", "source": map[string]string{"type": "text", "data": text}}}}
		messages := []any{first}
		if followup {
			messages = append(messages, map[string]string{"role": "assistant", "content": "read it"}, map[string]string{"role": "user", "content": "continue"})
		}
		raw, _ := json.Marshal(map[string]any{"model": "m", "messages": messages})
		req := httptest.NewRequest("POST", "https://example.test/v1/messages", strings.NewReader(string(raw)))
		if _, err := KiroRequest(req, "", "account-scope"); err != nil {
			t.Fatal(err)
		}
		out, _ := io.ReadAll(req.Body)
		return gjson.GetBytes(out, "conversationState.conversationId").String()
	}
	first := id("first document", false)
	if first != id("first document", true) {
		t.Fatal("later turns changed a document-only conversation anchor")
	}
	if first == id("second document", false) {
		t.Fatal("different documents shared a conversation anchor")
	}
}
