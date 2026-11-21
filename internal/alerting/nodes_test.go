package alerting

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPIHome/internal/cluster"
)

func TestNodeCandidatesUsesMembershipHeartbeat(t *testing.T) {
	ctx := context.Background()
	service, db := newTestService(t)
	now := dbNow(t, db)
	old := now.Add(-10 * time.Minute)
	settings := DefaultSettings()
	snapshot := cluster.CPANodeRecord{
		HomeIP: "home", HomePort: 8327, HomeStartedAt: old,
		NodeKey: "worker", NodeID: "worker", ClientIP: "10.0.0.9",
		CertificateFingerprint: "fingerprint", LastSeenAt: old,
	}
	membership := cluster.CPANodeMembershipRecord{
		CertificateFingerprint: "fingerprint", NodeID: "worker",
		State: cluster.MembershipStateActive, LastSeenAt: now,
	}
	for _, record := range []any{&snapshot, &membership, &NodeSeen{NodeID: "worker", LastSeenAt: old}} {
		if errCreate := db.Create(record).Error; errCreate != nil {
			t.Fatalf("seed %T: %v", record, errCreate)
		}
	}
	seedResolveState(t, service, "node_offline:worker", true)
	if errEval := service.Evaluate(ctx, settings); errEval != nil {
		t.Fatal(errEval)
	}
	if _, ok := firingKeys(t, service)["node_offline:worker"]; ok {
		t.Fatal("fresh membership must clear the alert despite a stale snapshot")
	}

	// A fresh connection snapshot must not mask missing subscription pongs.
	if errUpdate := db.Model(&membership).Update("last_seen_at", old).Error; errUpdate != nil {
		t.Fatal(errUpdate)
	}
	if errUpdate := db.Model(&snapshot).Update("last_seen_at", now.Add(10*time.Minute)).Error; errUpdate != nil {
		t.Fatal(errUpdate)
	}
	items, errCandidates := nodeCandidates(ctx, db, settings, now.Add(3*time.Minute))
	if errCandidates != nil {
		t.Fatal(errCandidates)
	}
	if len(items) != 1 || !strings.HasPrefix(items[0].Title, "Worker offline:") {
		t.Fatalf("missing heartbeat should fire a Worker alert, got %+v", items)
	}
}

func TestNodeCandidatesMembershipWithoutSnapshotAndMonotonicHistory(t *testing.T) {
	ctx := context.Background()
	_, db := newTestService(t)
	now := dbNow(t, db)
	settings := DefaultSettings()
	membership := cluster.CPANodeMembershipRecord{
		CertificateFingerprint: "fingerprint", NodeID: "worker",
		State: cluster.MembershipStateActive, LastSeenAt: now,
	}
	if errCreate := db.Create(&membership).Error; errCreate != nil {
		t.Fatal(errCreate)
	}
	items, errCandidates := nodeCandidates(ctx, db, settings, now)
	if errCandidates != nil || len(items) != 0 {
		t.Fatalf("live membership without snapshot: items=%+v, error=%v", items, errCandidates)
	}
	if errUpdate := db.Model(&membership).Update("last_seen_at", now.Add(-time.Hour)).Error; errUpdate != nil {
		t.Fatal(errUpdate)
	}
	items, errCandidates = nodeCandidates(ctx, db, settings, now)
	if errCandidates != nil || len(items) != 0 {
		t.Fatalf("older observation must not regress history: items=%+v, error=%v", items, errCandidates)
	}
	if errDelete := db.Delete(&membership).Error; errDelete != nil {
		t.Fatal(errDelete)
	}
	items, errCandidates = nodeCandidates(ctx, db, settings, now.Add(3*time.Minute))
	if errCandidates != nil || len(items) != 1 {
		t.Fatalf("disappeared membership must remain tracked: items=%+v, error=%v", items, errCandidates)
	}
}

func TestNodeCandidatesDoesNotRediscoverClosedMembership(t *testing.T) {
	ctx := context.Background()
	_, db := newTestService(t)
	now := dbNow(t, db)
	membership := cluster.CPANodeMembershipRecord{
		CertificateFingerprint: "old", NodeID: "decommissioned",
		State: cluster.MembershipStateClosed, LastSeenAt: now.Add(-time.Hour),
	}
	if errCreate := db.Create(&membership).Error; errCreate != nil {
		t.Fatal(errCreate)
	}
	items, errCandidates := nodeCandidates(ctx, db, DefaultSettings(), now)
	if errCandidates != nil || len(items) != 0 {
		t.Fatalf("closed membership must not reintroduce forgotten Workers: items=%+v, error=%v", items, errCandidates)
	}
}
