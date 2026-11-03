package migrations

import (
	"testing"

	"gorm.io/gorm"
)

// seedLegacyExplicitPolicies supplies the explicit inputs that populated upgrade
// fixtures had before the plan-owned cutover. It is used only before Up; the
// binding tables are absent from the latest schema.
func seedLegacyExplicitPolicies(t *testing.T, db *gorm.DB) {
	t.Helper()
	for _, statement := range []string{
		`INSERT INTO policy_versions(id,kind,code,version,source_id,config,status,effective_at,created_by_user_id) VALUES
   (9001,'pricing','migration-price','1',1,JSON_OBJECT(),'active','2000-01-01',1),
   (9002,'credit','migration-credit','1',1,JSON_OBJECT(),'active','2000-01-01',1),
   (9003,'gpu-idle','migration-idle','1',1,JSON_OBJECT(),'active','2000-01-01',1),
   (9004,'gpu-scheduling','migration-scheduling','1',NULL,JSON_OBJECT(),'active','2000-01-01',1),
   (9005,'metrics','migration-metrics','1',NULL,JSON_OBJECT(),'active','2000-01-01',1)`,
		`INSERT INTO platform_policy_bindings(kind,policy_id) SELECT kind,id FROM policy_versions WHERE id BETWEEN 9001 AND 9005`,
	} {
		if err := db.Exec(statement).Error; err != nil {
			t.Fatalf("seed legacy explicit policies: %v", err)
		}
	}
}
