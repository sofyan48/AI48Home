package auth

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	internalconfig "github.com/router-for-me/CLIProxyAPIHome/internal/config"
)

type dispatchQuotaResolver struct {
	auths   map[string]*Auth
	blocked map[string]bool

	failure error
	calls   []string
}

func (r *dispatchQuotaResolver) GetFullAuth(_ context.Context, id string) (*Auth, error) {
	return r.auths[id].Clone(), nil
}

func (r *dispatchQuotaResolver) CheckDispatchQuota(_ context.Context, id, model string) error {
	r.calls = append(r.calls, id+":"+model)
	if r.failure != nil {
		return r.failure
	}
	if r.blocked[id] {
		return &Error{Code: "quota_exhausted", HTTPStatus: 429}
	}
	return nil
}

type dispatchQuotaPlugin struct{}

func (dispatchQuotaPlugin) PickAuth(_ context.Context, req pluginapi.SchedulerPickRequest) (pluginapi.SchedulerPickResponse, bool, error) {
	return pluginapi.SchedulerPickResponse{Handled: true, AuthID: req.Candidates[0].ID}, true, nil
}

type dispatchQuotaStore struct {
	Store
	*dispatchQuotaResolver
}

func (s dispatchQuotaStore) MutateAuthState(_ context.Context, id string, mutate func(*Auth) StateMutation) (*Auth, error) {
	a := s.auths[id].Clone()
	mutate(a)
	s.auths[id] = a.Clone()
	return a, nil
}

func TestDispatchQuotaGuardIgnoresCooldownClear(t *testing.T) {
	manager := NewManager(nil, &FillFirstSelector{}, nil)
	a := &Auth{ID: "quota-cooling", Provider: "codex", Status: StatusActive}
	registerDispatchTestAuth(t, manager, a, "quota-cooling-model")
	resolver := &dispatchQuotaResolver{auths: map[string]*Auth{a.ID: a}, blocked: map[string]bool{a.ID: true}}
	// Exercise the optional store capability without a full resolver.
	manager.SetStore(dispatchQuotaStore{dispatchQuotaResolver: resolver})
	manager.SetConfig(&internalconfig.Config{DisableCooling: true})
	if errClear := manager.ClearDisabledCooldownStates(t.Context()); errClear != nil {
		t.Fatal(errClear)
	}
	if _, errClear := manager.ClearQuotaCooldown(t.Context(), a.ID, ""); errClear != nil {
		t.Fatal(errClear)
	}
	manager.SetStore(dispatchQuotaStore{dispatchQuotaResolver: resolver})
	decision, errPick := manager.Dispatch(t.Context(), []string{"codex"}, "quota-cooling-model", Options{})
	var authErr *Error
	if decision != nil || !errors.As(errPick, &authErr) || authErr.Code != "quota_exhausted" {
		t.Fatalf("cooldown clearing bypassed quota: %v %v", decision, errPick)
	}
}

func TestDispatchQuotaGuardAffinityRollback(t *testing.T) {
	for _, rebind := range []string{"quota", "never"} {
		t.Run(rebind, func(t *testing.T) {
			store := NewSessionCache(time.Hour)
			t.Cleanup(store.Stop)
			weighted := &WeightedRoundRobinSelector{}
			sticky := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{Store: store, Fallback: weighted, RebindOn: rebind})
			manager := NewManager(nil, sticky, nil)
			a := &Auth{ID: "rollback-a", Provider: "codex", Status: StatusActive}
			b := &Auth{ID: "rollback-b", Provider: "codex", Status: StatusActive}
			for _, candidate := range []*Auth{a, b} {
				registerDispatchTestAuth(t, manager, candidate, "quota-rollback")
			}
			resolver := &dispatchQuotaResolver{auths: map[string]*Auth{a.ID: a, b.ID: b}, blocked: map[string]bool{a.ID: true}}
			manager.SetFullAuthResolver(resolver)
			opts := Options{Headers: http.Header{"X-Session-Id": {"rollback-session"}}}
			primary, _ := extractSessionIDs(opts.Headers, nil, nil)
			key := sessionBindingKey("mixed", primary, "quota-rollback")
			store.Set(key, a.ID)
			before, errBefore := store.LoadSessionBinding(t.Context(), key)
			if errBefore != nil {
				t.Fatal(errBefore)
			}
			decision, errPick := manager.Dispatch(t.Context(), []string{"codex"}, "quota-rollback", opts)
			if rebind == "never" {
				var authErr *Error
				if decision != nil || !errors.As(errPick, &authErr) || authErr.Code != "quota_exhausted" {
					t.Fatalf("never binding bypassed: %v %v", decision, errPick)
				}
			} else if errPick != nil || decision == nil || decision.Auth.ID != b.ID {
				t.Fatalf("failover: %v %v", decision, errPick)
			}
			after, errAfter := store.LoadSessionBinding(t.Context(), key)
			if errAfter != nil || before != after {
				t.Fatalf("rejected selection changed binding: before=%+v after=%+v err=%v", before, after, errAfter)
			}
			for _, state := range weighted.states {
				if state.inFlight != 0 || len(state.pendingForTest()) != 0 {
					t.Fatal("quota skip leaked weighted reservation")
				}
			}
		})
	}
}

func TestDispatchQuotaGuardCanonicalAliasesWholeCredential(t *testing.T) {
	const (
		upstreamModel = "quota-upstream-shared"
		otherModel    = "quota-upstream-other"
	)
	for _, mode := range []string{"fast", "custom"} {
		for _, kind := range []string{"oauth", "api-key"} {
			for _, prefix := range []string{"", "team"} {
				t.Run(mode+"/"+kind+"/"+prefix, func(t *testing.T) {
					var selector Selector = &FillFirstSelector{}
					if mode == "custom" {
						selector = &credentialPolicyCandidateSelector{}
					}
					manager := NewManager(nil, selector, nil)
					t.Cleanup(manager.Shutdown)
					if got := manager.useSchedulerFastPath(); got != (mode == "fast") {
						t.Fatalf("useSchedulerFastPath() = %t for %s", got, mode)
					}
					a := &Auth{
						ID:       "quota-model-" + mode + "-" + kind + "-" + prefix,
						Provider: "xai", Prefix: prefix, Status: StatusActive,
						Attributes: map[string]string{"auth_kind": "oauth"},
					}
					if kind == "oauth" {
						manager.SetOAuthModelAlias(map[string][]internalconfig.OAuthModelAlias{"xai": {
							{Name: upstreamModel, Alias: "alias-a"},
							{Name: upstreamModel, Alias: "alias-b"},
							{Name: otherModel, Alias: "alias-other"},
						}})
					} else {
						a.Attributes = map[string]string{"auth_kind": "apikey", "api_key": "test-key"}
						manager.SetConfig(&internalconfig.Config{XAIKey: []internalconfig.XAIKey{{
							APIKey: "test-key", Prefix: prefix,
							Models: []internalconfig.XAIModel{
								{Name: upstreamModel, Alias: "alias-a"},
								{Name: upstreamModel, Alias: "alias-b"},
								{Name: otherModel, Alias: "alias-other"},
							},
						}}})
					}
					route := func(alias string) string {
						if prefix != "" {
							return prefix + "/" + alias
						}
						return alias
					}
					registerDispatchTestAuth(t, manager, a, route("alias-a"), route("alias-b"), route("alias-other"))
					resolver := &dispatchQuotaResolver{
						auths:   map[string]*Auth{a.ID: a.Clone()},
						blocked: map[string]bool{},
					}
					manager.SetFullAuthResolver(resolver)
					assertDispatch := func(alias, upstream string, blocked bool) {
						t.Helper()
						resolver.calls = nil
						decision, errDispatch := manager.Dispatch(t.Context(), []string{"xai"}, route(alias), Options{})
						if blocked {
							var authErr *Error
							if decision != nil || !errors.As(errDispatch, &authErr) || authErr.Code != "quota_exhausted" || authErr.HTTPStatus != http.StatusTooManyRequests {
								t.Fatalf("Dispatch(%q) = %v, %v; want quota_exhausted/429", route(alias), decision, errDispatch)
							}
						} else if errDispatch != nil || decision == nil || decision.Auth == nil || decision.Auth.ID != a.ID || decision.UpstreamModel != upstream {
							t.Fatalf("Dispatch(%q) = %v, %v; want auth %q and upstream %q", route(alias), decision, errDispatch, a.ID, upstream)
						}
						if want := []string{a.ID + ":" + upstream}; !reflect.DeepEqual(resolver.calls, want) {
							t.Fatalf("Dispatch(%q) quota calls = %v, want %v", route(alias), resolver.calls, want)
						}
					}
					assertDispatch("alias-a", upstreamModel, false)
					assertDispatch("alias-b", upstreamModel, false)
					before, _ := manager.GetByID(a.ID)
					resolver.blocked[a.ID] = true
					assertDispatch("alias-a", upstreamModel, true)
					assertDispatch("alias-b", upstreamModel, true)
					assertDispatch("alias-other", otherModel, true)
					assertDispatch("alias-a", upstreamModel, true)
					after, _ := manager.GetByID(a.ID)
					if !reflect.DeepEqual(before, after) {
						t.Fatal("credential-wide quota rejection mutated auth")
					}
				})
			}
		}
	}
}

func TestDispatchQuotaGuard(t *testing.T) {
	for _, mode := range []string{"fast", "custom", "weighted", "session", "plugin"} {
		t.Run(mode, func(t *testing.T) {
			var selector Selector = &FillFirstSelector{}
			switch mode {
			case "custom":
				selector = &credentialPolicyCandidateSelector{}
			case "weighted":
				selector = &WeightedRoundRobinSelector{}
			case "session":
				sticky := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{Fallback: &WeightedRoundRobinSelector{}})
				t.Cleanup(sticky.Stop)
				selector = sticky
			}
			manager := NewManager(nil, selector, nil)
			resolver := &dispatchQuotaResolver{auths: map[string]*Auth{}, blocked: map[string]bool{}}
			for _, suffix := range []string{"a", "b"} {
				id := mode + "-quota-" + suffix
				auth := &Auth{ID: id, Provider: "codex", Status: StatusActive, Attributes: map[string]string{"priority": "0"}}
				if suffix == "a" {
					auth.Attributes["priority"] = "10"
				}
				registerDispatchTestAuth(t, manager, auth, "quota-model")
				resolver.auths[id] = auth.Clone()
			}
			manager.SetFullAuthResolver(resolver)
			manager.SetConfig(&internalconfig.Config{RequestRetry: 2})
			if mode == "plugin" {
				manager.SetPluginScheduler(dispatchQuotaPlugin{})
			}
			a, b := mode+"-quota-a", mode+"-quota-b"
			resolver.blocked[a] = true
			opts := Options{Headers: http.Header{"X-Session-Id": {"quota-session"}}}
			pick := func(opts Options) (*DispatchDecision, error) {
				return manager.Dispatch(t.Context(), []string{"codex"}, "quota-model", opts)
			}
			before, _ := manager.GetByID(a)
			decision, errPick := pick(opts)
			if errPick != nil || decision == nil || decision.Auth.ID != b {
				t.Fatalf("skip: decision=%v err=%v", decision, errPick)
			}
			after, _ := manager.GetByID(a)
			if !reflect.DeepEqual(before, after) {
				t.Fatal("quota skip mutated auth")
			}
			resolver.blocked[b] = true
			assertQuota := func(opts Options, code string, status int) {
				t.Helper()
				decision, errPick := pick(opts)
				var authErr *Error
				if decision != nil || !errors.As(errPick, &authErr) || authErr.Code != code || authErr.HTTPStatus != status {
					t.Fatalf("decision=%v err=%v, want %s/%d", decision, errPick, code, status)
				}
			}
			assertQuota(opts, "quota_exhausted", 429)
			resolver.blocked[b] = false
			assertQuota(Options{Metadata: map[string]any{PinnedAuthIDMetadataKey: a}}, "quota_exhausted", 429)
			resolver.failure = errors.New("database down")
			assertQuota(opts, "quota_guard_unavailable", 503)
			resolver.failure = nil
			resolver.blocked[a] = false
			decision, errPick = pick(Options{Metadata: map[string]any{PinnedAuthIDMetadataKey: a, RequestRetryRoundMetadataKey: 1, requestRetryDefaultMetadataKey: 2}})
			if errPick != nil || decision == nil || decision.Auth.ID != a {
				t.Fatalf("recovery/retry: decision=%v err=%v", decision, errPick)
			}
			if len(resolver.calls) == 0 {
				t.Fatal("quota guard not called")
			}
		})
	}
}
