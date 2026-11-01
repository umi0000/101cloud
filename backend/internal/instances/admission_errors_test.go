package instances

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"os"
	"testing"

	"101cloud/backend/internal/platform"
	driver "github.com/go-sql-driver/mysql"
	"gorm.io/gorm"
)

func TestAdmissionCapacityDetailsPreserveIntegers(t *testing.T) {
	err := admissionCapacityError("quota_exceeded", "user_quota", "disk_bytes", "磁盘", "bytes", 9007199254740993, 20, 9007199254741000, "请减少申请量。")
	var api *platform.HTTPError
	if !errors.As(err, &api) || api.Status != 409 || api.Code != "quota_exceeded" {
		t.Fatalf("unexpected error: %v", err)
	}
	for key, want := range map[string]string{"scope": "user_quota", "resource": "disk_bytes", "used": "9007199254740993", "requested": "20", "limit": "9007199254741000", "available": "7", "unit": "bytes"} {
		if api.Details.(platform.Row)[key] != want {
			t.Fatalf("%s: got %v, want %s", key, api.Details.(platform.Row)[key], want)
		}
	}
}

// Exercise the actual admission aggregation against MySQL, including JSON NULL
// semantics. This isolated schema contains only columns read by reserveExisting.
func admissionMySQLFixture(t *testing.T) (*gorm.DB, *platform.App) {
	t.Helper()
	dsn := os.Getenv("CLOUD_TEST_MYSQL_ADMIN_DSN")
	if dsn == "" {
		t.Skip("set CLOUD_TEST_MYSQL_ADMIN_DSN for isolated admission regression")
	}
	cfg, err := driver.ParseDSN(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.DBName = ""
	owner, err := sql.Open("mysql", cfg.FormatDSN())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { owner.Close() })
	var nonce [8]byte
	if _, err = rand.Read(nonce[:]); err != nil {
		t.Fatal(err)
	}
	name := "ci_admission_" + hex.EncodeToString(nonce[:])
	if _, err = owner.Exec("CREATE DATABASE `" + name + "`"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { owner.Exec("DROP DATABASE `" + name + "`") })
	cfg.DBName = name
	db, err := platform.OpenDB(platform.Config{DSN: cfg.FormatDSN()})
	if err != nil {
		t.Fatal(err)
	}
	connection, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { connection.Close() })
	for _, statement := range []string{
		"CREATE TABLE users (id BIGINT PRIMARY KEY,status VARCHAR(20))",
		"CREATE TABLE user_limits (user_id BIGINT PRIMARY KEY,max_instances BIGINT,max_vcpus BIGINT,max_memory_bytes BIGINT,max_disk_bytes BIGINT,max_resident_instances BIGINT)",
		"CREATE TABLE nodes (id BIGINT PRIMARY KEY,administrative_state VARCHAR(20),health VARCHAR(20),connection_verified_at DATETIME(6),resources_sampled_at DATETIME(6),allocatable_vcpus BIGINT,allocatable_memory_bytes BIGINT,allocatable_disk_bytes BIGINT,resident_vcpu_budget BIGINT,resident_memory_budget_bytes BIGINT,resident_disk_budget_bytes BIGINT)",
		"CREATE TABLE instances (id BIGINT PRIMARY KEY,user_id BIGINT,node_id BIGINT,product_type VARCHAR(20),desired_state VARCHAR(20),observed_state VARCHAR(20),provisioning_state VARCHAR(20),incus_uuid VARCHAR(100),incus_config_snapshot JSON,vcpus BIGINT,memory_bytes BIGINT,disk_bytes BIGINT,deleted_at DATETIME(6),revision BIGINT)",
		"INSERT INTO users VALUES(1,'active')",
		"INSERT INTO user_limits VALUES(1,20,20,20,20,20)",
		"INSERT INTO nodes VALUES(1,'active','online',NOW(6),NOW(6),20,20,20,20,20,20)",
		"INSERT INTO instances VALUES(1,1,1,'custom','running','error','failed',NULL,NULL,4,4,4,NULL,1),(2,1,1,'custom','running','error','failed',NULL,JSON_OBJECT('creation_reservations_released',true),4,4,4,NULL,1)",
	} {
		if err = db.Exec(statement).Error; err != nil {
			t.Fatal(err)
		}
	}
	return db, platform.New(db, platform.Config{})
}

func TestMySQLAdmissionDimensionsAndReleasedReservations(t *testing.T) {
	db, a := admissionMySQLFixture(t)
	var err error
	request := platform.Row{"user_id": uint64(1), "node_id": uint64(1), "product_type": "custom", "vcpus": uint64(2), "memory_bytes": uint64(2), "disk_bytes": uint64(2)}
	for _, tc := range []struct{ table, column, scope, resource, used, want, limit string }{
		{"user_limits", "max_instances", "user_quota", "instances", "1", "1", "1"},
		{"user_limits", "max_vcpus", "user_quota", "vcpus", "4", "2", "5"},
		{"user_limits", "max_memory_bytes", "user_quota", "memory_bytes", "4", "2", "5"},
		{"user_limits", "max_disk_bytes", "user_quota", "disk_bytes", "4", "2", "5"},
		{"user_limits", "max_resident_instances", "user_quota", "resident_instances", "1", "1", "1"},
		{"nodes", "allocatable_vcpus", "node_capacity", "vcpus", "4", "2", "5"},
		{"nodes", "allocatable_memory_bytes", "node_capacity", "memory_bytes", "4", "2", "5"},
		{"nodes", "allocatable_disk_bytes", "node_capacity", "disk_bytes", "4", "2", "5"},
		{"nodes", "resident_vcpu_budget", "resident_capacity", "vcpus", "4", "2", "5"},
		{"nodes", "resident_memory_budget_bytes", "resident_capacity", "memory_bytes", "4", "2", "5"},
		{"nodes", "resident_disk_budget_bytes", "resident_capacity", "disk_bytes", "4", "2", "5"},
	} {
		t.Run(tc.column, func(t *testing.T) {
			if err := db.Table(tc.table).Where("1=1").UpdateColumn(tc.column, tc.limit).Error; err != nil {
				t.Fatal(err)
			}
			defer db.Table(tc.table).Where("1=1").UpdateColumn(tc.column, 20)
			err := reserveExisting(context.Background(), a, db, request, nil)
			var api *platform.HTTPError
			if !errors.As(err, &api) {
				t.Fatalf("expected dimension error, got %v", err)
			}
			wantCode := map[string]string{"user_quota": "quota_exceeded", "node_capacity": "node_capacity_unavailable", "resident_capacity": "resident_capacity_unavailable"}[tc.scope]
			if api.Status != 409 || api.Code != wantCode {
				t.Fatalf("unexpected status/code: %v", err)
			}
			details := api.Details.(platform.Row)
			unit := "bytes"
			if tc.resource == "vcpus" || tc.resource == "instances" || tc.resource == "resident_instances" {
				unit = "count"
			}
			if details["unit"] != unit || details["action"] == nil || details["action"] == "" {
				t.Fatalf("missing unit/action: %v", details)
			}
			for key, want := range map[string]string{"scope": tc.scope, "resource": tc.resource, "used": tc.used, "requested": tc.want, "limit": tc.limit} {
				if api.Details.(platform.Row)[key] != want {
					t.Fatalf("%s: got %v, want %s (%v)", key, api.Details.(platform.Row)[key], want, err)
				}
			}
			var count int64
			if err := db.Table("instances").Where("revision=1 AND deleted_at IS NULL").Count(&count).Error; err != nil || count != 2 {
				t.Fatalf("rejection mutated existing records: %d %v", count, err)
			}
		})
	}
	if err = db.Table("user_limits").Where("user_id=1").UpdateColumn("max_vcpus", 5).Error; err != nil {
		t.Fatal(err)
	}
	for _, snapshot := range []string{"NULL", "JSON_OBJECT()", "JSON_OBJECT('creation_reservations_released',false)"} {
		if err = db.Exec("UPDATE instances SET incus_config_snapshot=" + snapshot + " WHERE id=1").Error; err != nil {
			t.Fatal(err)
		}
		err = reserveExisting(context.Background(), a, db, request, nil)
		var api *platform.HTTPError
		if !errors.As(err, &api) || api.Code != "quota_exceeded" || api.Details.(platform.Row)["used"] != "4" {
			t.Fatalf("legacy failure stopped consuming quota (%s): %v", snapshot, err)
		}
	}
	if err = db.Exec("UPDATE instances SET incus_config_snapshot=JSON_OBJECT('creation_reservations_released',true) WHERE id=1").Error; err != nil {
		t.Fatal(err)
	}
	if err = reserveExisting(context.Background(), a, db, request, nil); err != nil {
		t.Fatalf("safe released failure still consumes quota: %v", err)
	}
	for _, change := range []string{"provisioning_state='unknown'", "incus_uuid='native-still-exists'"} {
		if err = db.Exec("UPDATE instances SET provisioning_state='failed',incus_uuid=NULL WHERE id=1").Error; err != nil {
			t.Fatal(err)
		}
		if err = db.Exec("UPDATE instances SET " + change + " WHERE id=1").Error; err != nil {
			t.Fatal(err)
		}
		var api *platform.HTTPError
		err = reserveExisting(context.Background(), a, db, request, nil)
		if !errors.As(err, &api) || api.Code != "quota_exceeded" {
			t.Fatalf("unsafe record excluded (%s): %v", change, err)
		}
	}
}
