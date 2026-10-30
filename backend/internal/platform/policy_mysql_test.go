package platform

import (
	"strconv"
	"testing"
)

// External version IDs are policy_versions IDs; the legacy source row ID must
// translate in both directions, and an ID of another kind is rejected.
func TestMySQLPolicyIDTranslatesLegacySource(t *testing.T) {
	db := jobTestDB(t)
	var users []uint64
	if e := db.Table("users").Order("id").Limit(1).Pluck("id", &users).Error; e != nil || len(users) == 0 {
		t.Skip("a user fixture is required")
	}
	const source = uint64(9000000000000000001)
	r := Row{"kind": "gpu-pricing", "code": "regression-policy-id", "version": "1", "source_id": source, "config": "{}", "status": "draft", "created_by_user_id": users[0]}
	if e := db.Table("policy_versions").Create(&r).Error; e != nil {
		t.Fatal(e)
	}
	var ids []uint64
	if e := db.Table("policy_versions").Where("kind='gpu-pricing' AND source_id=?", source).Pluck("id", &ids).Error; e != nil || len(ids) != 1 {
		t.Fatalf("inserted policy not found: %v", e)
	}
	want := strconv.FormatUint(ids[0], 10)
	got, e := PolicyID(db, "gpu-pricing", source)
	if e != nil || got != want {
		t.Fatalf("PolicyID=%v,%v want %s", got, e, want)
	}
	if got, e = PolicyID(db, "gpu-pricing", nil); e != nil || got != nil {
		t.Fatalf("empty source must stay null: %v,%v", got, e)
	}
	if _, back, e := PolicySource(db, "gpu-pricing", want, false); e != nil || back != source {
		t.Fatalf("PolicySource=%d,%v want %d", back, e, source)
	}
	if _, _, e := PolicySource(db, "pricing", want, false); e == nil || e.(*HTTPError).Status != 422 {
		t.Fatalf("policy of another kind accepted: %v", e)
	}
}
