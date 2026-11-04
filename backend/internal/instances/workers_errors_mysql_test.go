package instances

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"encoding/hex"
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
	"github.com/gin-gonic/gin"
	mysqlDriver "github.com/go-sql-driver/mysql"
	"gorm.io/gorm"
)

// Each fixture owns an isolated schema. Real tables support financial ledger
// self-joins, which MySQL disallows for connection-local TEMPORARY tables.
func creationCleanupDB(t *testing.T, handlers ...http.Handler) (*gorm.DB, *platform.App, platform.Job, platform.Row, *atomic.Int64) {
	t.Helper()
	dsn := os.Getenv("CLOUD_TEST_MYSQL_ADMIN_DSN")
	if dsn == "" {
		t.Skip("set CLOUD_TEST_MYSQL_ADMIN_DSN for isolated creation cleanup regressions")
	}
	cfg, err := mysqlDriver.ParseDSN(dsn)
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
	name := "ci_creation_cleanup_" + hex.EncodeToString(nonce[:])
	if _, err = owner.ExecContext(t.Context(), "CREATE DATABASE `"+name+"`"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { owner.Exec("DROP DATABASE `" + name + "`") })
	cfg.DBName = name
	db, err := platform.OpenDB(platform.Config{DSN: cfg.FormatDSN()})
	if err != nil {
		t.Fatal(err)
	}
	pool, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pool.Close() })
	definitions := map[string]string{
		"credit_accounts":              "id BIGINT PRIMARY KEY,user_id BIGINT,status VARCHAR(24),credit_policy_version_id BIGINT DEFAULT 1,version BIGINT DEFAULT 0,revision BIGINT DEFAULT 0,settlement_pending BOOL NOT NULL DEFAULT FALSE",
		"credit_buckets":               "id BIGINT PRIMARY KEY,account_id BIGINT,remaining_pt DECIMAL(20,6),reserved_pt DECIMAL(20,6),valid_from DATETIME(6),expires_at DATETIME(6),version BIGINT DEFAULT 0,revision BIGINT DEFAULT 0",
		"credit_ledger":                "id BIGINT AUTO_INCREMENT PRIMARY KEY,account_id BIGINT,bucket_id BIGINT,business_key VARCHAR(191) UNIQUE,entry_type VARCHAR(24),amount_delta_pt DECIMAL(20,6),reserved_delta_pt DECIMAL(20,6),remaining_after_pt DECIMAL(20,6),reserved_after_pt DECIMAL(20,6),reservation_id BIGINT,charge_id BIGINT,metadata_redacted JSON",
		"credit_reservations":          "id BIGINT PRIMARY KEY,account_id BIGINT,session_id BIGINT,bucket_id BIGINT,reserved_pt DECIMAL(20,6),settled_pt DECIMAL(20,6),released_pt DECIMAL(20,6),status VARCHAR(24),closed_at DATETIME(6),revision BIGINT DEFAULT 0",
		"usage_sessions":               "id BIGINT AUTO_INCREMENT PRIMARY KEY,account_id BIGINT,user_id BIGINT,node_id BIGINT,instance_id BIGINT,charge_kind VARCHAR(24),metering_state VARCHAR(24),status VARCHAR(24),metered_microseconds BIGINT DEFAULT 0,cumulative_expected_pt DECIMAL(20,6) DEFAULT 0,started_at DATETIME(6),last_settled_at DATETIME(6),ended_at DATETIME(6),revision BIGINT DEFAULT 0,pricing_version_id BIGINT,billing_mode VARCHAR(32),config_snapshot JSON,cpu_hourly_pt DECIMAL(20,6) DEFAULT 0,memory_gib_hourly_pt DECIMAL(20,6) DEFAULT 0,disk_gib_hourly_pt DECIMAL(20,6) DEFAULT 0,gpu_hourly_pt DECIMAL(20,6) DEFAULT 0",
		"usage_charges":                "id BIGINT AUTO_INCREMENT PRIMARY KEY,session_id BIGINT,account_id BIGINT,from_at DATETIME(6),to_at DATETIME(6),expected_pt DECIMAL(20,6),settled_pt DECIMAL(20,6),status VARCHAR(24),settled_at DATETIME(6),calculation_snapshot JSON,revision BIGINT DEFAULT 0,UNIQUE(session_id,from_at)",
		"tasks":                        "id BIGINT AUTO_INCREMENT PRIMARY KEY,task_type VARCHAR(64),user_id BIGINT,node_id BIGINT,instance_id BIGINT,status VARCHAR(24),phase VARCHAR(64),parameters_redacted JSON,error_code VARCHAR(64),finished_at DATETIME(6),lease_owner VARCHAR(128),lease_generation BIGINT,lease_until DATETIME(6),result_redacted JSON,revision BIGINT DEFAULT 0,idempotency_scope VARCHAR(64),idempotency_key VARCHAR(128),is_instance_mutation BOOL,priority INT",
		"task_steps":                   "id BIGINT AUTO_INCREMENT PRIMARY KEY,task_id BIGINT,step_key VARCHAR(128),attempt BIGINT,status VARCHAR(24),error_code VARCHAR(64),operation_id VARCHAR(128),node_id BIGINT,project_name VARCHAR(64),parameters_redacted JSON,result_redacted JSON,started_at DATETIME(6),finished_at DATETIME(6),UNIQUE(task_id,step_key,attempt)",
		"instances":                    "id BIGINT PRIMARY KEY,user_id BIGINT,node_id BIGINT,project_name VARCHAR(64),incus_name VARCHAR(64),incus_uuid VARCHAR(64),image_type VARCHAR(24),image_id BIGINT,image_fingerprint VARCHAR(64),architecture VARCHAR(24),vcpus BIGINT,memory_bytes BIGINT,disk_bytes BIGINT,storage_pool VARCHAR(64),desired_state VARCHAR(24),observed_state VARCHAR(24),provisioning_state VARCHAR(24),observed_at DATETIME(6),deleted_at DATETIME(6),creation_failed_at DATETIME(6),creation_failure_task_id BIGINT,creation_cleanup_state VARCHAR(16),incus_config_snapshot JSON,revision BIGINT DEFAULT 0,billing_mode VARCHAR(32) DEFAULT 'admin_exempt',credit_policy_version_id BIGINT,init_template_version_id BIGINT,cpu_hourly_pt DECIMAL(20,6) DEFAULT 0,memory_gib_hourly_pt DECIMAL(20,6) DEFAULT 0,disk_gib_hourly_pt DECIMAL(20,6) DEFAULT 0,product_type VARCHAR(24),allowed_user_actions JSON,logical_name VARCHAR(64),login_config_ciphertext MEDIUMBLOB,login_initialized_at DATETIME(6)",
		"ip_addresses":                 "id BIGINT PRIMARY KEY,instance_id BIGINT,node_id BIGINT,node_network_binding_id BIGINT,network_pool_id BIGINT,address_family INT,address BLOB,status VARCHAR(24),reservation_task_id BIGINT,reserved_at DATETIME(6),reservation_expires_at DATETIME(6),bound_at DATETIME(6),revision BIGINT DEFAULT 0",
		"gpu_allocations":              "id BIGINT PRIMARY KEY,instance_id BIGINT,user_id BIGINT,node_id BIGINT,status VARCHAR(24),active_slot INT GENERATED ALWAYS AS (CASE WHEN status IN ('reserved','attaching','active','releasing','unknown') THEN 1 ELSE NULL END) STORED,configured BOOL DEFAULT FALSE,observed BOOL DEFAULT FALSE,attached_at DATETIME(6),released_at DATETIME(6),release_reason VARCHAR(255),release_verified BOOL DEFAULT FALSE,last_verified_at DATETIME(6),revision BIGINT DEFAULT 0",
		"nodes":                        "id BIGINT PRIMARY KEY,agent_endpoint VARCHAR(255),token_ciphertext BLOB,server_cert_sha256 BLOB,default_project_name VARCHAR(64) DEFAULT 'default',administrative_state VARCHAR(24),health VARCHAR(24),connection_verified_at DATETIME(6),resources_sampled_at DATETIME(6),allocatable_vcpus BIGINT DEFAULT 100,allocatable_memory_bytes BIGINT DEFAULT 1099511627776,allocatable_disk_bytes BIGINT DEFAULT 1099511627776,resident_vcpu_budget BIGINT DEFAULT 100,resident_memory_budget_bytes BIGINT DEFAULT 1099511627776,resident_disk_budget_bytes BIGINT DEFAULT 1099511627776",
		"node_network_bindings":        "id BIGINT PRIMARY KEY,node_id BIGINT,network_pool_id BIGINT DEFAULT 1,project_name VARCHAR(64),status VARCHAR(24),validation_status VARCHAR(24),mode VARCHAR(24),parent_interface VARCHAR(64),incus_network_name VARCHAR(64),mtu INT",
		"network_pools":                "id BIGINT PRIMARY KEY,prefix_length INT,dns_servers JSON,gateway BLOB,network_address BLOB,status VARCHAR(24) DEFAULT 'active'",
		"images":                       "id BIGINT PRIMARY KEY,source_type VARCHAR(24),fingerprint BINARY(32),architecture VARCHAR(24),image_type VARCHAR(24),status VARCHAR(24),gpu_ready BOOL,properties JSON",
		"image_nodes":                  "id BIGINT PRIMARY KEY,image_id BIGINT,node_id BIGINT,project_name VARCHAR(64),fingerprint BINARY(32),architecture VARCHAR(24),cloud_init BOOL,gpu_ready BOOL,properties JSON,status VARCHAR(24),variant VARCHAR(64),source_id VARCHAR(24),revision BIGINT DEFAULT 1,last_verified_at DATETIME(6)",
		"policy_versions":              "id BIGINT PRIMARY KEY,kind VARCHAR(24),source_id BIGINT,status VARCHAR(24),effective_at DATETIME(6),config JSON",
		"credit_enforcements":          "id BIGINT PRIMARY KEY,account_id BIGINT,status VARCHAR(24),policy_snapshot JSON,exhausted_at DATETIME(6)",
		"credit_enforcement_instances": "id BIGINT AUTO_INCREMENT PRIMARY KEY,enforcement_id BIGINT,instance_id BIGINT,task_id BIGINT,status VARCHAR(24) DEFAULT 'pending',revision BIGINT DEFAULT 1,UNIQUE(enforcement_id,instance_id)",
		"users":                        "id BIGINT PRIMARY KEY,status VARCHAR(24)",
		"user_limits":                  "user_id BIGINT PRIMARY KEY,max_instances BIGINT,max_vcpus BIGINT,max_memory_bytes BIGINT,max_disk_bytes BIGINT,max_resident_instances BIGINT",
	}
	// DDL precedes the worker transaction so it cannot invalidate savepoints.
	for table, definition := range definitions {
		if err = db.Exec("CREATE TABLE `" + table + "` (" + definition + ") ENGINE=InnoDB").Error; err != nil {
			t.Fatal(err)
		}
	}
	tx := db.Begin()
	if tx.Error != nil {
		t.Fatal(tx.Error)
	}
	t.Cleanup(func() { tx.Rollback() })
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
	instance := platform.Row{"id": uint64(15), "user_id": uint64(2), "node_id": uint64(1), "project_name": "default", "incus_name": "test-pending", "image_type": "container", "image_id": 1, "image_fingerprint": strings.Repeat("a", 64), "architecture": "x86_64", "vcpus": 1, "memory_bytes": 1073741824, "disk_bytes": 21474836480, "storage_pool": "default", "credit_policy_version_id": 1, "desired_state": "running", "observed_state": "pending", "provisioning_state": "pending", "incus_config_snapshot": "{}"}
	seeds := map[string][]platform.Row{
		"nodes":                 {{"id": 1, "agent_endpoint": server.URL, "token_ciphertext": token, "server_cert_sha256": fingerprint[:]}},
		"instances":             {instance},
		"tasks":                 {{"id": 43, "task_type": "instance.create", "user_id": 2, "node_id": 1, "instance_id": 15, "status": "running", "lease_owner": "test", "lease_generation": 1, "lease_until": at.Add(time.Minute), "result_redacted": "{}"}},
		"credit_accounts":       {{"id": 7, "user_id": 2, "status": "active"}},
		"credit_buckets":        {{"id": 1, "account_id": 7, "remaining_pt": "10.000000", "reserved_pt": "2.000000", "valid_from": at.Add(-time.Hour)}},
		"usage_sessions":        {{"id": 1, "account_id": 7, "user_id": 2, "node_id": 1, "instance_id": 15, "charge_kind": "base_compute", "metering_state": "held", "status": "active", "started_at": at, "last_settled_at": at}, {"id": 2, "account_id": 7, "user_id": 2, "node_id": 1, "instance_id": 15, "charge_kind": "storage", "metering_state": "running", "status": "active", "started_at": at, "last_settled_at": at}},
		"credit_reservations":   {{"id": 1, "account_id": 7, "session_id": 1, "bucket_id": 1, "reserved_pt": "1.000000", "settled_pt": "0", "released_pt": "0", "status": "held"}, {"id": 2, "account_id": 7, "session_id": 2, "bucket_id": 1, "reserved_pt": "1.000000", "settled_pt": "0", "released_pt": "0", "status": "held"}},
		"ip_addresses":          {{"id": 1, "instance_id": 15, "node_id": 1, "node_network_binding_id": 1, "network_pool_id": 1, "address_family": 4, "address": []byte{10, 0, 0, 2}, "status": "reserved", "reservation_task_id": 43, "reserved_at": at}},
		"gpu_allocations":       {{"id": 1, "instance_id": 15, "user_id": 2, "node_id": 1, "status": "reserved"}},
		"node_network_bindings": {{"id": 1, "node_id": 1, "project_name": "default", "status": "active", "validation_status": "verified", "mode": "bridge", "parent_interface": "br0"}},
		"network_pools":         {{"id": 1, "prefix_length": 24, "dns_servers": "[]"}},
		"images":                {{"id": 1, "source_type": "local", "fingerprint": []byte(strings.Repeat("b", 32)), "architecture": "aarch64", "image_type": "container", "status": "active", "gpu_ready": false, "properties": "{}"}},
		"image_nodes":           {{"id": 1, "image_id": 1, "node_id": 1, "project_name": "default", "fingerprint": []byte(strings.Repeat("\xaa", 32)), "architecture": "x86_64", "cloud_init": true, "gpu_ready": true, "properties": "{}", "status": "ready", "last_verified_at": at}},
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
	if err := db.Table("instances").Where("id=15").Update("billing_mode", "credit").Error; err != nil {
		t.Fatal(err)
	}
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
	if platform.Text(instance["provisioning_state"]) != "failed" || platform.Text(instance["observed_state"]) != "error" || instance["deleted_at"] == nil || instance["creation_failed_at"] == nil || platform.Text(instance["creation_cleanup_state"]) != "completed" || !platform.Bool(object(instance["incus_config_snapshot"])["creation_reservations_released"]) {
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
	for _, scenario := range []string{"native_intent", "reconcile_lineage", "recovery", "lease_expired", "ip_owner_changed", "gpu_configured"} {
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

func TestMySQLCreateWorkerUsesFrozenLocalImageWithoutCatalogLookup(t *testing.T) {
	var requests atomic.Int64
	db, a, _, instance, _ := creationCleanupDB(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Method != http.MethodGet || r.URL.Path != "/1.0/networks/br0" || r.URL.Query().Get("project") != "tenant-project" {
			http.Error(w, "unexpected image lookup, pull, or project", 400)
			return
		}
		_ = json.NewEncoder(w).Encode(platform.Row{"metadata": platform.Row{"config": platform.Row{"ipv4.nat": "false", "ipv6.nat": "false"}}})
	}))
	if err := db.Table("node_network_bindings").Where("id=1").Updates(platform.Row{"project_name": "tenant-project", "mode": "managed_bridge", "incus_network_name": "br0"}).Error; err != nil {
		t.Fatal(err)
	}
	instance["project_name"] = "tenant-project"
	// Removing catalog metadata proves neither image availability nor provenance
	// can change the already-admitted instance's local pinned create source.
	if err := db.Exec("DELETE FROM images").Error; err != nil {
		t.Fatal(err)
	}
	body, err := workerCreateBody(t.Context(), a, instance)
	if err != nil {
		t.Fatal(err)
	}
	source := object(body["source"])
	if len(source) != 2 || platform.Text(source["type"]) != "image" || platform.Text(source["fingerprint"]) != strings.Repeat("a", 64) || platform.Text(body["architecture"]) != "x86_64" || requests.Load() != 1 {
		t.Fatalf("worker did not use frozen local source: %+v, requests=%d", body, requests.Load())
	}
}

func TestMySQLImportVerifiesSelectedProjectImage(t *testing.T) {
	db, a, _, _, _ := creationCleanupDB(t)
	for _, statement := range []string{
		`UPDATE node_network_bindings SET project_name='tenant-project' WHERE id=1`,
		`UPDATE network_pools SET network_address=INET6_ATON('10.0.0.0') WHERE id=1`,
		`UPDATE image_nodes SET project_name='tenant-project' WHERE id=1`,
	} {
		if err := db.Exec(statement).Error; err != nil {
			t.Fatal(err)
		}
	}
	spec := Spec{ImageID: "1", NetworkOptionID: "1"}
	native := platform.Row{"architecture": "x86_64", "config": platform.Row{"volatile.base_image": strings.Repeat("a", 64)}, "devices": platform.Row{"eth0": platform.Row{"type": "nic", "nictype": "bridged", "parent": "br0"}}}
	if err := workerImportNetwork(t.Context(), a, 1, spec, native, "tenant-project", "10.0.0.2"); err != nil {
		t.Fatalf("node-specific native image rejected in nondefault project: %v", err)
	}
	native["config"] = platform.Row{"volatile.base_image": strings.Repeat("62", 32)}
	if err := workerImportNetwork(t.Context(), a, 1, spec, native, "tenant-project", "10.0.0.2"); err == nil {
		t.Fatal("import accepted the global image fingerprint instead of the selected project's fingerprint")
	}
	if err := db.Table("image_nodes").Where("id=1").Update("status", "pending").Error; err != nil {
		t.Fatal(err)
	}
	native["config"] = platform.Row{"volatile.base_image": strings.Repeat("a", 64)}
	if err := workerImportNetwork(t.Context(), a, 1, spec, native, "tenant-project", "10.0.0.2"); err == nil {
		t.Fatal("import accepted an unprepared image")
	}
}

func TestMySQLCustomValidationUsesNetworkProjectGPUFlag(t *testing.T) {
	db, a, _, _, _ := creationCleanupDB(t)
	for _, statement := range []string{
		`UPDATE node_network_bindings SET project_name='tenant-project' WHERE id=1`,
		`UPDATE image_nodes SET project_name='tenant-project' WHERE id=1`,
	} {
		if err := db.Exec(statement).Error; err != nil {
			t.Fatal(err)
		}
	}
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/", nil)
	count, model := 1, "NVIDIA A100"
	fields := customConfigFields{InstanceType: "container", ImageID: "1", NetworkOptionID: "1", Resources: Resources{VCPUs: 1, MemoryBytes: "1073741824", DiskBytes: "21474836480"}, InitParameters: map[string]any{}}
	gpu := customGPUConfig{Count: &count, Model: &model, AccessMode: "exclusive", MaxHourlyPT: "3"}
	if err := validateCustomFields(c, a, fields, gpu, "1"); err != nil {
		t.Fatalf("custom validation used logical image GPU flag instead of selected project: %v", err)
	}
	if err := db.Table("image_nodes").Where("id=1").Update("gpu_ready", false).Error; err != nil {
		t.Fatal(err)
	}
	if err := validateCustomFields(c, a, fields, gpu, "1"); err == nil {
		t.Fatal("custom validation accepted a node image without GPU approval")
	}
}

func TestMySQLImageTaskReconcileWithoutInstance(t *testing.T) {
	db, a, job, _, _ := creationCleanupDB(t)
	job.Type, job.InstanceID = "instance.reconcile", 0
	job.Parameters = json.RawMessage(`{"source_task_id":"44"}`)
	if err := db.Table("tasks").Where("id=43").Updates(platform.Row{"task_type": job.Type, "instance_id": nil, "parameters_redacted": string(job.Parameters)}).Error; err != nil {
		t.Fatal(err)
	}
	source := platform.Row{"id": 44, "task_type": "image.prepare", "user_id": 2, "node_id": 1, "instance_id": nil, "status": "unknown", "lease_generation": 1, "parameters_redacted": `{"image_node_id":"7"}`, "result_redacted": "{}"}
	if err := db.Table("tasks").Create(&source).Error; err != nil {
		t.Fatal(err)
	}
	called := false
	a.RegisterReconciler("image.prepare", func(ctx context.Context, app *platform.App, original platform.Job) error {
		called = true
		if original.InstanceID != 0 || original.ID != job.ID || original.Type != "image.prepare" || platform.Uint(object(string(original.Parameters))["source_task_id"]) != 44 {
			t.Fatalf("noninstance reconcile identity changed: %+v", original)
		}
		return nil
	})
	if err := runReconcile(t.Context(), a, job); err != nil {
		t.Fatal(err)
	}
	row, err := platform.Get(db, "tasks", 44, false)
	if err != nil || !called || platform.Text(row["status"]) != "succeeded" || platform.Uint(row["instance_id"]) != 0 {
		t.Fatalf("image reconcile did not complete noninstance source: %+v, %v", row, err)
	}
}

func TestMySQLImageTaskDTOExposesOnlySafeProgress(t *testing.T) {
	db, _, _, _, _ := creationCleanupDB(t)
	row := platform.Row{"id": 43, "task_type": "image.prepare", "instance_id": nil, "status": "running", "result_redacted": `{"progress":"42%","status":"downloading","image_node_id":"7","fingerprint":"abc","architecture":"x86_64","operation_id":"private","token":"secret","error":{"raw":"secret"}}`}
	dto, err := taskDTO(db, row)
	if err != nil {
		t.Fatal(err)
	}
	result := object(dto["result"])
	if len(result) != 5 || result["progress"] != "42%" || result["status"] != "downloading" || result["image_node_id"] != "7" || result["operation_id"] != nil || result["token"] != nil || result["error"] != nil {
		t.Fatalf("unsafe or missing image progress: %+v", result)
	}
	row["task_type"] = "instance.create"
	row["instance_id"] = uint64(15)
	dto, err = taskDTO(db, row)
	if err != nil || dto["result"] != nil {
		t.Fatalf("instance task exposed arbitrary result: %+v %v", dto, err)
	}
}

func TestMySQLImageReconcilePreservesWorkerFailureClassification(t *testing.T) {
	for _, uncertain := range []bool{false, true} {
		name := "known_failure"
		if uncertain {
			name = "uncertain_dispatch"
		}
		t.Run(name, func(t *testing.T) {
			db, a, job, _, _ := creationCleanupDB(t)
			job.Type, job.InstanceID = "instance.reconcile", 0
			job.Parameters = json.RawMessage(`{"source_task_id":"44"}`)
			if err := db.Table("tasks").Where("id=43").Updates(platform.Row{"task_type": job.Type, "instance_id": nil, "parameters_redacted": string(job.Parameters)}).Error; err != nil {
				t.Fatal(err)
			}
			source := platform.Row{"id": 44, "task_type": "image.prepare", "user_id": 2, "node_id": 1, "instance_id": nil, "status": "unknown", "lease_generation": 1, "parameters_redacted": `{"image_node_id":"7"}`, "result_redacted": "{}"}
			if err := db.Table("tasks").Create(&source).Error; err != nil {
				t.Fatal(err)
			}
			var outcome error = platform.E(409, "image_prepare_failed", "Image preparation failed.")
			if uncertain {
				outcome = platform.Unknown(outcome)
			}
			a.RegisterReconciler("image.prepare", func(context.Context, *platform.App, platform.Job) error { return outcome })
			if err := runReconcile(t.Context(), a, job); err != outcome {
				t.Fatalf("wrapper changed worker classification: got %T %v, want %T %v", err, err, outcome, outcome)
			}
		})
	}
}
