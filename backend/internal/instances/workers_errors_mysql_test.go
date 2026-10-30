package instances

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"101cloud/backend/internal/platform"
	"gorm.io/gorm"
)

// Every fixture table is connection-local: no persistent rows, quotas or nodes
// are altered even when the test DSN points at an existing migrated test database.
func creationCleanupDB(t *testing.T, handlers ...http.Handler) (*gorm.DB, *platform.App, platform.Job, platform.Row, *atomic.Int64) {
	t.Helper()
	dsn := os.Getenv("CLOUD_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("set CLOUD_TEST_MYSQL_DSN to a migrated test database")
	}
	db, err := platform.OpenDB(platform.Config{DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	pool, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pool.Close() })
	tx := db.Begin()
	if tx.Error != nil {
		t.Fatal(tx.Error)
	}
	t.Cleanup(func() { tx.Rollback() })
	definitions := map[string]string{
		"credit_accounts":       "id BIGINT PRIMARY KEY,user_id BIGINT,status VARCHAR(24),credit_policy_version_id BIGINT DEFAULT 1,version BIGINT DEFAULT 0,revision BIGINT DEFAULT 0",
		"credit_buckets":        "id BIGINT PRIMARY KEY,account_id BIGINT,remaining_pt DECIMAL(20,6),reserved_pt DECIMAL(20,6),version BIGINT DEFAULT 0,revision BIGINT DEFAULT 0",
		"credit_ledger":         "id BIGINT AUTO_INCREMENT PRIMARY KEY,account_id BIGINT,bucket_id BIGINT,business_key VARCHAR(191) UNIQUE,entry_type VARCHAR(24),amount_delta_pt DECIMAL(20,6),reserved_delta_pt DECIMAL(20,6),remaining_after_pt DECIMAL(20,6),reserved_after_pt DECIMAL(20,6),reservation_id BIGINT",
		"credit_reservations":   "id BIGINT PRIMARY KEY,account_id BIGINT,session_id BIGINT,bucket_id BIGINT,reserved_pt DECIMAL(20,6),settled_pt DECIMAL(20,6),released_pt DECIMAL(20,6),status VARCHAR(24),closed_at DATETIME(6),revision BIGINT DEFAULT 0",
		"usage_sessions":        "id BIGINT PRIMARY KEY,account_id BIGINT,user_id BIGINT,node_id BIGINT,instance_id BIGINT,charge_kind VARCHAR(24),metering_state VARCHAR(24),status VARCHAR(24),metered_microseconds BIGINT DEFAULT 0,cumulative_expected_pt DECIMAL(20,6) DEFAULT 0,started_at DATETIME(6),last_settled_at DATETIME(6),ended_at DATETIME(6),revision BIGINT DEFAULT 0",
		"usage_charges":         "id BIGINT PRIMARY KEY,session_id BIGINT",
		"tasks":                 "id BIGINT PRIMARY KEY,task_type VARCHAR(64),user_id BIGINT,node_id BIGINT,instance_id BIGINT,status VARCHAR(24),phase VARCHAR(64),parameters_redacted JSON,error_code VARCHAR(64),finished_at DATETIME(6),lease_owner VARCHAR(128),lease_generation BIGINT,lease_until DATETIME(6),result_redacted JSON,revision BIGINT DEFAULT 0",
		"task_steps":            "id BIGINT AUTO_INCREMENT PRIMARY KEY,task_id BIGINT,step_key VARCHAR(128),attempt BIGINT,status VARCHAR(24),operation_id VARCHAR(128),node_id BIGINT,project_name VARCHAR(64),parameters_redacted JSON,result_redacted JSON,started_at DATETIME(6),finished_at DATETIME(6),UNIQUE(task_id,step_key,attempt)",
		"instances":             "id BIGINT PRIMARY KEY,user_id BIGINT,node_id BIGINT,project_name VARCHAR(64),incus_name VARCHAR(64),incus_uuid VARCHAR(64),image_type VARCHAR(24),image_id BIGINT,image_fingerprint VARCHAR(64),architecture VARCHAR(24),vcpus BIGINT,memory_bytes BIGINT,disk_bytes BIGINT,storage_pool VARCHAR(64),desired_state VARCHAR(24),observed_state VARCHAR(24),provisioning_state VARCHAR(24),observed_at DATETIME(6),deleted_at DATETIME(6),incus_config_snapshot JSON,revision BIGINT DEFAULT 0",
		"ip_addresses":          "id BIGINT PRIMARY KEY,instance_id BIGINT,node_id BIGINT,node_network_binding_id BIGINT,network_pool_id BIGINT,address_family INT,address BLOB,status VARCHAR(24),reservation_task_id BIGINT,reserved_at DATETIME(6),reservation_expires_at DATETIME(6),bound_at DATETIME(6),revision BIGINT DEFAULT 0",
		"gpu_allocations":       "id BIGINT PRIMARY KEY,instance_id BIGINT,user_id BIGINT,node_id BIGINT,status VARCHAR(24),active_slot INT GENERATED ALWAYS AS (CASE WHEN status IN ('reserved','attaching','active','releasing','unknown') THEN 1 ELSE NULL END) STORED,configured BOOL DEFAULT FALSE,observed BOOL DEFAULT FALSE,attached_at DATETIME(6),released_at DATETIME(6),release_reason VARCHAR(255),release_verified BOOL DEFAULT FALSE,last_verified_at DATETIME(6),revision BIGINT DEFAULT 0",
		"nodes":                 "id BIGINT PRIMARY KEY,agent_endpoint VARCHAR(255),token_ciphertext BLOB,server_cert_sha256 BLOB,default_project_name VARCHAR(64) DEFAULT 'default'",
		"node_network_bindings": "id BIGINT PRIMARY KEY,node_id BIGINT,project_name VARCHAR(64),status VARCHAR(24),validation_status VARCHAR(24),mode VARCHAR(24),parent_interface VARCHAR(64),mtu INT",
		"network_pools":         "id BIGINT PRIMARY KEY,prefix_length INT,dns_servers JSON,gateway BLOB",
		"images":                "id BIGINT PRIMARY KEY,source_type VARCHAR(24)",
		"policy_versions":       "id BIGINT PRIMARY KEY,kind VARCHAR(24),source_id BIGINT,status VARCHAR(24),effective_at DATETIME(6),config JSON",
	}
	for table, definition := range definitions {
		if err = tx.Exec("CREATE TEMPORARY TABLE `" + table + "` (" + definition + ") ENGINE=InnoDB").Error; err != nil {
			t.Fatal(err)
		}
	}
	mutations := &atomic.Int64{}
	handler := http.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			mutations.Add(1)
			http.Error(w, "unexpected mutation", 500)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(r.URL.Path, "/storage-pools/"):
			_ = json.NewEncoder(w).Encode(platform.Row{"metadata": platform.Row{"space": platform.Row{"total": "20401094656", "used": "4294967296"}}})
		case r.URL.Path == "/1.0/instances":
			_ = json.NewEncoder(w).Encode(platform.Row{"metadata": []platform.Row{}})
		default:
			_ = json.NewEncoder(w).Encode(platform.Row{"metadata": platform.Row{}})
		}
	}))
	if len(handlers) > 0 {
		handler = handlers[0]
	}
	server := httptest.NewUnstartedServer(handler)
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	certificate := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}}
	der, err := x509.CreateCertificate(rand.Reader, certificate, certificate, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	server.TLS = &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}
	server.StartTLS()
	t.Cleanup(server.Close)
	a := platform.New(tx, platform.Config{MasterKey: make([]byte, 32), NodeAllowCIDRs: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}})
	token, err := a.Encrypt([]byte(strings.Repeat("A", 43)))
	if err != nil {
		t.Fatal(err)
	}
	fingerprint := sha256.Sum256(server.Certificate().Raw)
	job := platform.Job{ID: 43, Type: "instance.create", InstanceID: 15, UserID: 2, NodeID: 1, LeaseGeneration: 1}
	at := a.Time()
	instance := platform.Row{"id": uint64(15), "user_id": uint64(2), "node_id": uint64(1), "project_name": "default", "incus_name": "test-pending", "image_type": "container", "image_id": 1, "image_fingerprint": strings.Repeat("a", 64), "architecture": "x86_64", "vcpus": 1, "memory_bytes": 1073741824, "disk_bytes": 21474836480, "storage_pool": "default", "desired_state": "running", "observed_state": "pending", "provisioning_state": "pending", "incus_config_snapshot": "{}"}
	seeds := map[string][]platform.Row{
		"nodes":                 {{"id": 1, "agent_endpoint": server.URL, "token_ciphertext": token, "server_cert_sha256": fingerprint[:]}},
		"instances":             {instance},
		"tasks":                 {{"id": 43, "task_type": "instance.create", "user_id": 2, "node_id": 1, "instance_id": 15, "status": "running", "lease_owner": "test", "lease_generation": 1, "lease_until": at.Add(time.Minute), "result_redacted": "{}"}},
		"credit_accounts":       {{"id": 7, "user_id": 2, "status": "active"}},
		"credit_buckets":        {{"id": 1, "account_id": 7, "remaining_pt": "10.000000", "reserved_pt": "2.000000"}},
		"usage_sessions":        {{"id": 1, "account_id": 7, "user_id": 2, "node_id": 1, "instance_id": 15, "charge_kind": "base_compute", "metering_state": "held", "status": "active", "started_at": at, "last_settled_at": at}, {"id": 2, "account_id": 7, "user_id": 2, "node_id": 1, "instance_id": 15, "charge_kind": "storage", "metering_state": "running", "status": "active", "started_at": at, "last_settled_at": at}},
		"credit_reservations":   {{"id": 1, "account_id": 7, "session_id": 1, "bucket_id": 1, "reserved_pt": "1.000000", "settled_pt": "0", "released_pt": "0", "status": "held"}, {"id": 2, "account_id": 7, "session_id": 2, "bucket_id": 1, "reserved_pt": "1.000000", "settled_pt": "0", "released_pt": "0", "status": "held"}},
		"ip_addresses":          {{"id": 1, "instance_id": 15, "node_id": 1, "node_network_binding_id": 1, "network_pool_id": 1, "address_family": 4, "address": []byte{10, 0, 0, 2}, "status": "reserved", "reservation_task_id": 43, "reserved_at": at}},
		"gpu_allocations":       {{"id": 1, "instance_id": 15, "user_id": 2, "node_id": 1, "status": "reserved"}},
		"node_network_bindings": {{"id": 1, "node_id": 1, "project_name": "default", "status": "active", "validation_status": "verified", "mode": "bridge", "parent_interface": "br0"}},
		"network_pools":         {{"id": 1, "prefix_length": 24, "dns_servers": "[]"}},
		"images":                {{"id": 1, "source_type": "local"}},
		"policy_versions":       {{"id": 1, "kind": "credit", "source_id": 1, "status": "active", "effective_at": at.Add(-time.Hour), "config": "{}"}},
	}
	for table, records := range seeds {
		for _, record := range records {
			if err = tx.Table(table).Create(&record).Error; err != nil {
				t.Fatal(err)
			}
		}
	}
	return tx, a, job, instance, mutations
}

func TestMySQLKnownDiskShortageCancelsOnlyPristineCreation(t *testing.T) {
	db, a, job, _, mutations := creationCleanupDB(t)
	err := runCreate(context.Background(), a, job)
	if err == nil || platform.PublicError(err, false)["code"] != "storage_pool_capacity_exceeded" {
		t.Fatalf("concrete failure missing: %v", err)
	}
	if _, known := err.(*platform.HTTPError); !known {
		t.Fatalf("known pre-dispatch shortage became uncertain: %T", err)
	}
	if mutations.Load() != 0 {
		t.Fatal("preflight dispatched a mutation")
	}
	instance, _ := platform.Get(db, "instances", 15, false)
	if platform.Text(instance["provisioning_state"]) != "failed" || platform.Text(instance["observed_state"]) != "error" || instance["deleted_at"] != nil || !platform.Bool(object(instance["incus_config_snapshot"])["creation_reservations_released"]) {
		t.Fatalf("failed creation state/marker wrong: %+v", instance)
	}
	ip, _ := platform.Get(db, "ip_addresses", 1, false)
	gpu, _ := platform.Get(db, "gpu_allocations", 1, false)
	bucket, _ := platform.Get(db, "credit_buckets", 1, false)
	if platform.Text(ip["status"]) != "available" || ip["instance_id"] != nil || platform.Text(gpu["status"]) != "cancelled" || platform.Text(bucket["reserved_pt"]) != "0.000000" || platform.Text(bucket["remaining_pt"]) != "10.000000" {
		t.Fatalf("reservations not safely released: ip=%+v gpu=%+v bucket=%+v", ip, gpu, bucket)
	}
	var entries, active, native int64
	if err := db.Table("credit_ledger").Where("entry_type='release'").Count(&entries).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Table("usage_sessions").Where("status='active'").Count(&active).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Table("task_steps").Where("step_key='create.native'").Count(&native).Error; err != nil {
		t.Fatal(err)
	}
	if entries != 2 || active != 0 || native != 0 {
		t.Fatalf("financial history/native journal wrong: entries=%d active=%d native=%d", entries, active, native)
	}
	view := instanceDTO(a, instance)
	if view["resource_reservations_released"] != true {
		t.Fatal("released state absent from instance DTO")
	}
	storedTask, _ := platform.Get(db, "tasks", job.ID, false)
	if !platform.Bool(object(storedTask["result_redacted"])["creation_reservations_released"]) {
		t.Fatal("task release marker absent")
	}
}

func TestMySQLUncertainCreationRetainsAllReservations(t *testing.T) {
	for _, scenario := range []string{"native_intent", "reconcile_lineage", "recovery", "lease_expired", "ip_owner_changed", "gpu_configured", "metered", "charge_interval", "ledger_failure"} {
		t.Run(scenario, func(t *testing.T) {
			db, a, job, instance, mutations := creationCleanupDB(t)
			ctx := context.Background()
			var err error
			switch scenario {
			case "native_intent":
				err = db.Table("task_steps").Create(&platform.Row{"task_id": 43, "step_key": "create.native", "attempt": 1, "status": "running", "parameters_redacted": "{}"}).Error
			case "reconcile_lineage":
				err = db.Table("tasks").Create(&platform.Row{"id": 44, "task_type": "instance.reconcile", "instance_id": 15, "user_id": 2, "node_id": 1, "status": "unknown"}).Error
			case "recovery":
				ctx = context.WithValue(ctx, workerRecoveryKey{}, []uint64{42})
			case "lease_expired":
				err = db.Table("tasks").Where("id=43").Update("lease_until", a.Time().Add(-time.Second)).Error
			case "ip_owner_changed":
				err = db.Table("ip_addresses").Where("id=1").Update("reservation_task_id", 99).Error
			case "gpu_configured":
				err = db.Table("gpu_allocations").Where("id=1").Update("configured", true).Error
			case "metered":
				err = db.Table("usage_sessions").Where("id=2").Update("metered_microseconds", 1).Error
			case "charge_interval":
				err = db.Table("usage_charges").Create(&platform.Row{"id": 1, "session_id": 2}).Error
			case "ledger_failure":
				err = db.Table("credit_ledger").Create(&platform.Row{"business_key": "release:2"}).Error
			}
			if err != nil {
				t.Fatal(err)
			}
			cause := storageCapacityError(map[string]any{"total": "20401094656", "used": "4294967296"}, "default", 21474836480)
			err = workerCreatePreflightFailed(ctx, a, job, instance, cause)
			if err == nil || platform.PublicError(err, true)["code"] != "storage_pool_capacity_exceeded" {
				t.Fatalf("uncertain concrete cause missing: %v", err)
			}
			if _, known := err.(*platform.HTTPError); known {
				t.Fatal("unsafe cleanup refusal became known failure")
			}
			ip, _ := platform.Get(db, "ip_addresses", 1, false)
			gpu, _ := platform.Get(db, "gpu_allocations", 1, false)
			bucket, _ := platform.Get(db, "credit_buckets", 1, false)
			current, _ := platform.Get(db, "instances", 15, false)
			if platform.Text(ip["status"]) != "reserved" || platform.Text(gpu["status"]) != "reserved" || platform.Text(bucket["reserved_pt"]) != "2.000000" || platform.Bool(object(current["incus_config_snapshot"])["creation_reservations_released"]) || current["deleted_at"] != nil || mutations.Load() != 0 {
				t.Fatalf("unsafe partial cleanup: ip=%+v gpu=%+v bucket=%+v instance=%+v mutations=%d", ip, gpu, bucket, current, mutations.Load())
			}
		})
	}
}

func TestMySQLAmbiguousNativeIntentNeverReplaysOrReleases(t *testing.T) {
	db, a, job, _, mutations := creationCleanupDB(t)
	if err := db.Table("task_steps").Create(&platform.Row{"task_id": 43, "step_key": "create.native", "attempt": 1, "status": "running", "parameters_redacted": "{}"}).Error; err != nil {
		t.Fatal(err)
	}
	err := runCreate(context.Background(), a, job)
	if err == nil {
		t.Fatal("unconfirmed native submission became successful")
	}
	if _, known := err.(*platform.HTTPError); known {
		t.Fatal("ambiguous native submission became known failure")
	}
	current, _ := platform.Get(db, "instances", 15, false)
	bucket, _ := platform.Get(db, "credit_buckets", 1, false)
	if platform.Text(current["provisioning_state"]) != "unknown" || platform.Bool(object(current["incus_config_snapshot"])["creation_reservations_released"]) || platform.Text(bucket["reserved_pt"]) != "2.000000" || mutations.Load() != 0 {
		t.Fatalf("ambiguous submission replayed or released: instance=%+v bucket=%+v mutations=%d", current, bucket, mutations.Load())
	}
}
