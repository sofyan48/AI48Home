package auth

import (
	"context"
	"errors"
	"net/http"
)

// DispatchQuotaChecker is an optional store/resolver capability. It reads durable
// quota eligibility independently of cooldowns and must not mutate auth state.
type DispatchQuotaChecker interface {
	CheckDispatchQuota(ctx context.Context, credentialID, model string) error
}

func (m *Manager) checkDispatchQuota(ctx context.Context, credentialID, model string) *Error {
	m.mu.RLock()
	checker, ok := m.fullResolver.(DispatchQuotaChecker)
	if !ok {
		checker, ok = m.store.(DispatchQuotaChecker)
	}
	m.mu.RUnlock()
	if !ok {
		return nil
	}
	errCheck := checker.CheckDispatchQuota(ctx, credentialID, model)
	if errCheck == nil {
		return nil
	}
	var quotaErr *Error
	if errors.As(errCheck, &quotaErr) && quotaErr.Code == "quota_exhausted" {
		return &Error{Code: "quota_exhausted", Message: "credential quota is exhausted for this model; authoritative recovery is required", HTTPStatus: http.StatusTooManyRequests}
	}
	// Unknown failures are not evidence that a credential is eligible. Do not
	// expose backend errors, which may include connection details.
	return &Error{Code: "quota_guard_unavailable", Message: "durable quota guard is unavailable", Retryable: true, HTTPStatus: http.StatusServiceUnavailable}
}

func quotaDispatchUnavailable(err error) bool {
	var authErr *Error
	if errors.As(err, &authErr) {
		switch authErr.Code {
		case "auth_not_found", "auth_unavailable", "session_auth_unavailable":
			return true
		}
	}
	var cooldown interface{ ErrorCode() string }
	return errors.As(err, &cooldown) && cooldown.ErrorCode() == "model_cooldown"
}
