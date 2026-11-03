// Package accountpool owns single-process account pool admission and state.
package accountpool

import (
	"net/http"
	"time"
)

type Error struct {
	Status  int
	Code    string
	Message string
}

func (err *Error) Error() string { return err.Message }

var (
	ErrProxyUnavailable = &Error{http.StatusBadGateway, "pool_proxy_unavailable", "the selected account outbound proxy is unavailable"}
	ErrNotBound         = &Error{http.StatusForbidden, "pool_not_bound", "the access token has no valid account pool binding"}
	ErrScope            = &Error{http.StatusForbidden, "pool_scope_violation", "the requested account is outside the access token's pool"}
	ErrUnavailable      = &Error{http.StatusServiceUnavailable, "pool_unavailable", "the account pool is unavailable"}
	ErrModel            = &Error{http.StatusBadRequest, "pool_model_unavailable", "the account pool does not support this model or protocol"}
	ErrQuota            = &Error{http.StatusTooManyRequests, "pool_quota_exhausted", "all eligible pool accounts have exhausted their quota"}
	ErrQueueFull        = &Error{http.StatusTooManyRequests, "pool_queue_full", "the account pool waiting queue is full"}
	ErrBusy             = &Error{http.StatusTooManyRequests, "pool_busy", "timed out waiting for account capacity"}
	ErrAffinity         = &Error{http.StatusConflict, "pool_affinity_unavailable", "the bound account cannot continue this request"}
	ErrChanged          = &Error{http.StatusForbidden, "pool_scope_changed", "the access token routing scope changed while the request was waiting"}
)

// QuotaExhausted carries advisory retry evidence, not permission to resume.
// An account still needs fresh usage evidence or its single recovery probe.
type QuotaExhausted struct{ RetryAt time.Time }

func (err *QuotaExhausted) Error() string { return ErrQuota.Error() }
func (err *QuotaExhausted) Unwrap() error { return ErrQuota }
