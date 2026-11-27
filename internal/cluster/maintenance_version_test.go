package cluster

import "testing"

func TestCombinedProviderNamespaceMigrationVersion(t *testing.T) {
	if currentDatabaseVersion != 31 {
		t.Fatalf("combined fork v30/upstream v21 migration must use v31, got %d", currentDatabaseVersion)
	}
	for version := 1; version <= currentDatabaseVersion; version++ {
		if _, ok := databaseSnapshotModels(version); !ok {
			t.Fatalf("lost snapshot compatibility for version %d", version)
		}
	}
	oldModels, _ := databaseSnapshotModels(30)
	newModels, _ := databaseSnapshotModels(31)
	if len(oldModels) != len(newModels) {
		t.Fatal("provider-key data migration changed snapshot table count")
	}
	for i := range oldModels {
		if oldModels[i].name != newModels[i].name {
			t.Fatalf("provider-key migration reordered table %d", i)
		}
	}
}
