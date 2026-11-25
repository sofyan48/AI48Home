package home

import (
	"context"
	"net/http"

	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
)

// quotaCheckedResolver keeps database-backed Home dispatch fail closed when an
// enabled adapter cannot supply the authoritative quota capability.
type quotaCheckedResolver struct {
	ClusterAdapter
}

func (r quotaCheckedResolver) CheckDispatchQuota(ctx context.Context, credentialID, model string) error {
	if checker, ok := r.ClusterAdapter.(coreauth.DispatchQuotaChecker); ok {
		return checker.CheckDispatchQuota(ctx, credentialID, model)
	}
	return &coreauth.Error{Code: "quota_guard_unavailable", Message: "durable quota guard is unavailable", Retryable: true, HTTPStatus: http.StatusServiceUnavailable}
}
