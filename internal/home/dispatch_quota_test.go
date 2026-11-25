package home

import (
	"context"
	"errors"
	"testing"

	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
)

type homeQuotaTestAdapter struct {
	ClusterAdapter
	credentialID string
	model        string
	err          error
}

func (a *homeQuotaTestAdapter) CheckDispatchQuota(_ context.Context, credentialID, model string) error {
	a.credentialID, a.model = credentialID, model
	return a.err
}

func TestHomeDispatchQuotaResolver(t *testing.T) {
	for _, code := range []string{"", "quota_exhausted", "quota_guard_unavailable"} {
		t.Run(code, func(t *testing.T) {
			adapter := &homeQuotaTestAdapter{}
			if code != "" {
				adapter.err = &coreauth.Error{Code: code}
			}
			errCheck := (quotaCheckedResolver{ClusterAdapter: adapter}).CheckDispatchQuota(t.Context(), "credential", "upstream")
			if errCheck != adapter.err || adapter.credentialID != "credential" || adapter.model != "upstream" {
				t.Fatalf("forwarded check = %v, adapter = %+v", errCheck, adapter)
			}
		})
	}
	var authErr *coreauth.Error
	errCheck := (quotaCheckedResolver{ClusterAdapter: &runtimeKVTestAdapter{enabled: true}}).CheckDispatchQuota(t.Context(), "credential", "upstream")
	if !errors.As(errCheck, &authErr) || authErr.Code != "quota_guard_unavailable" || authErr.HTTPStatus != 503 {
		t.Fatalf("missing capability must fail closed: %v", errCheck)
	}
}
