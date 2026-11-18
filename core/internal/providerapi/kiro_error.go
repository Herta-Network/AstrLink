package providerapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/MoeForge/moego/ojson"
)

// ErrKiroThinkingUnsupported identifies invalid or unrecognized reasoning history. The ingress repair policy decides whether to remove it.
var ErrKiroThinkingUnsupported = errors.New("Kiro cannot replay this thinking history")

// KiroStreamError retains structured provider fields for local risk handling,
// while Error never echoes the provider's potentially sensitive message.
type KiroStreamError struct {
	Status int
	Body   []byte
}

func (*KiroStreamError) Error() string { return "Kiro upstream returned an event-stream exception" }

func kiroStreamError(headers map[string]string, event ojson.Result) error {
	kind := headers[":exception-type"]
	if kind == "" {
		kind = headers[":error-code"]
	}
	if kind == "" {
		kind = headers[":event-type"]
	}
	status := http.StatusBadGateway
	if _, name, ok := strings.Cut(kind, "#"); ok {
		kind = name
	}
	switch strings.ToLower(kind) {
	case "throttlingexception":
		status = http.StatusTooManyRequests
	case "servicequotaexceededexception":
		status = http.StatusPaymentRequired
	case "internalserverexception", "internalservererror":
		status = http.StatusInternalServerError
	case "accessdeniedexception", "accountsuspendedexception":
		status = http.StatusForbidden
	case "unauthorizedexception", "unrecognizedclientexception", "expiredtokenexception":
		status = http.StatusUnauthorized
	case "serviceunavailableerror", "serviceunavailableexception":
		status = http.StatusServiceUnavailable
	case "validationerror", "validationexception":
		status = http.StatusBadRequest
	}
	message := event.Get("message").String()
	if message == "" {
		message = headers[":error-message"]
	}
	return newKiroStreamError(status, kind, event.Get("reason").String(), message)
}

// Kiro's SSE errors use either code or type; the SDK normalizes its documented
// Responses codes to the reasons used by its GAR stream.
func kiroResponsesError(e ojson.Result) error {
	code, kind := e.Get("code").String(), e.Get("type").String()
	reason := strings.ToUpper(strings.TrimPrefix(code, "kiro:"))
	// The explicit codes take precedence over the coarse error type.
	switch code {
	case "context_length_exceeded":
		reason = "CONTENT_LENGTH_EXCEEDS_THRESHOLD"
	case "model_not_found":
		reason = "INVALID_MODEL_ID"
	case "invalid_function_parameters":
		reason = "TOOL_SCHEMA_INVALID"
	case "invalid_json":
		reason = "REQUEST_BODY_INVALID_JSON"
	case "server_is_overloaded":
		reason = "MODEL_TEMPORARILY_UNAVAILABLE"
	case "rate_limit_exceeded":
		reason = "USER_REQUEST_RATE_EXCEEDED"
	}
	status := kiroResponsesStatus(reason, kind)

	return newKiroStreamError(status, kind, reason, e.Get("message").String())
}

func kiroResponsesStatus(reason, kind string) int {
	switch reason {
	case "CONTENT_LENGTH_EXCEEDS_THRESHOLD":
		return http.StatusRequestEntityTooLarge
	case "INVALID_MODEL_ID":
		return http.StatusNotFound
	case "TOOL_SCHEMA_INVALID", "REQUEST_BODY_INVALID_JSON":
		return http.StatusBadRequest
	case "ACCESS_DENIED", "MODEL_REQUIRES_USER_OPT_IN", "TEMPORARILY_SUSPENDED", "FEATURE_NOT_SUPPORTED", "UNAUTHORIZED_CUSTOMIZATION_RESOURCE_ACCESS", "UNAUTHORIZED_WORKSPACE_CONTEXT_FEATURE_ACCESS":
		return http.StatusForbidden
	case "CONVERSATION_LIMIT_EXCEEDED", "MONTHLY_REQUEST_COUNT", "OVERAGE_REQUEST_LIMIT_EXCEEDED", "CREDIT_BALANCE_EXHAUSTED":
		return http.StatusPaymentRequired
	case "CREDIT_CONSUMPTION_RATE_EXCEEDED", "DAILY_REQUEST_COUNT", "USER_REQUEST_RATE_EXCEEDED", "SERVICE_REQUEST_RATE_EXCEEDED", "INSUFFICIENT_MODEL_CAPACITY":
		return http.StatusTooManyRequests
	case "MODEL_TEMPORARILY_UNAVAILABLE":
		return http.StatusServiceUnavailable
	}
	if kind == "" {
		kind = strings.ToLower(reason)
	}
	switch kind {
	case "invalid_request", "invalid_request_error":
		return http.StatusBadRequest
	case "not_found":
		return http.StatusNotFound
	case "too_many_requests":
		return http.StatusTooManyRequests
	case "insufficient_quota":
		return http.StatusPaymentRequired
	}
	if strings.HasPrefix(reason, "INVALID_") || strings.HasPrefix(reason, "UNSUPPORTED_") || strings.HasPrefix(reason, "MISSING_") {
		return http.StatusBadRequest
	}
	return http.StatusInternalServerError
}

func newKiroStreamError(status int, kind, reason, message string) error {
	if len(kind) > 128 {
		kind = ""
	}
	if len(reason) > 128 {
		reason = ""
	}
	if len(message) > 4096 {
		message = "Kiro rejected the request"
	}
	body, _ := json.Marshal(map[string]string{"__type": kind, "message": message, "reason": reason})
	return &KiroStreamError{Status: status, Body: body}
}
