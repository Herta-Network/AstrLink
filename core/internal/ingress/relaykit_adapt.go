package ingress

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"

	"github.com/MoeForge/moego/ojson"
	"github.com/QuantumNous/astrlink/core/contract"
)

func adaptRelayKitRequest(
	request *http.Request,
	protocol contract.ProtocolID,
	streaming bool,
	model string,
	body []byte,
) error {
	if model == "" {
		return fmt.Errorf("relaykit upstream model is required")
	}
	path, geminiSuffix := "", ""
	query := url.Values{}
	switch protocol {
	case contract.ProtocolOpenAIChat:
		path = "/v1/chat/completions"
	case contract.ProtocolOpenAIResponses:
		path = "/v1/responses"
	case contract.ProtocolAnthropicMessages:
		path = "/v1/messages"
		if request.Header.Get("anthropic-version") == "" {
			request.Header.Set("anthropic-version", "2023-06-01")
		}
	case contract.ProtocolGoogleGenerateContent:
		geminiSuffix = ":generateContent"
		if streaming {
			geminiSuffix = ":streamGenerateContent"
			query.Set("alt", "sse")
		}
	default:
		return fmt.Errorf("unsupported relaykit upstream protocol %q", protocol)
	}
	if geminiSuffix != "" {
		setGeminiModelPath(request.URL, model, geminiSuffix)
	} else {
		request.URL.Path, request.URL.RawPath = path, ""
	}
	request.URL.RawQuery = query.Encode()
	request.Header.Del("Accept-Encoding")
	request.Header.Del("Content-Length")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Content-Length", strconv.Itoa(len(body)))
	request.ContentLength = int64(len(body))
	request.TransferEncoding = nil
	request.Body = io.NopCloser(bytes.NewReader(body))
	request.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(body)), nil
	}
	return nil
}

// RelayKit may introduce a Messages default, and a gateway reasoning rewrite
// may raise it. Only the caller's own limit belongs on the GAR wire.
func preserveKiroOutputLimit(protocol contract.ProtocolID, source, converted []byte) ([]byte, error) {
	var limit ojson.Result
	input := ojson.ParseBytes(source)
	switch protocol {
	case contract.ProtocolAnthropicMessages:
		limit = input.Get("max_tokens")
	case contract.ProtocolOpenAICompletions:
		limit = input.Get("max_completion_tokens")
		if !limit.Exists() || limit.Type == ojson.Null {
			limit = input.Get("max_tokens")
		}
	case contract.ProtocolGoogleGenerateContent:
		limit = input.Get("generationConfig.maxOutputTokens")
		if !limit.Exists() {
			limit = input.Get("generation_config.max_output_tokens")
		}
	default:
		return converted, nil
	}
	if !limit.Exists() || limit.Type == ojson.Null {
		return ojson.DeleteBytes(converted, "max_tokens")
	}
	return ojson.SetRawBytes(converted, "max_tokens", []byte(limit.Raw))
}
