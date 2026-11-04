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
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"101cloud/backend/internal/catalog"
	"101cloud/backend/internal/platform"
	migrations "101cloud/backend/sql"
	"github.com/gin-gonic/gin"
	driver "github.com/go-sql-driver/mysql"
	"gorm.io/gorm"
)

// Unlike the worker-only temporary-table fixtures, this exercises create against
// every real migration, foreign key, billing reservation and mutation transaction.
// Only an isolated database is seeded; no existing database rows are modified.
type planConfigurationFixture struct {
	t             *testing.T
	db            *gorm.DB
	a             *platform.App
	router        *gin.Engine
	requests      int
	requestUserID uint64
	reads         atomic.Int64
}

func newPlanConfigurationFixture(t *testing.T) *planConfigurationFixture {
	t.Helper()
	dsn := os.Getenv("CLOUD_TEST_MYSQL_ADMIN_DSN")
	if dsn == "" {
		t.Skip("set CLOUD_TEST_MYSQL_ADMIN_DSN for fully migrated elastic plan admission regressions")
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
	name := "ci_plan_admission_" + hex.EncodeToString(nonce[:])
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	if _, err = owner.ExecContext(ctx, "CREATE DATABASE `"+name+"`"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := owner.Exec("DROP DATABASE `" + name + "`"); err != nil {
			t.Errorf("drop isolated database: %v", err)
		}
	})
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
	if err = migrations.Up(ctx, db); err != nil {
		t.Fatal(err)
	}
	f := &planConfigurationFixture{t: t, db: db}
	// Admission is allowed precisely one read-only native operation. Unexpected
	// requests fail closed instead of returning permissive empty native evidence.
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/1.0/storage-pools/default/resources" || r.URL.Query().Get("project") != "default" {
			t.Errorf("unexpected admission Agent request: %s %s", r.Method, r.URL)
			http.Error(w, "unexpected native operation", http.StatusInternalServerError)
			return
		}
		f.reads.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(platform.Row{"metadata": platform.Row{"space": platform.Row{"total": "1099511627776", "used": "0"}}})
	}))
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
	f.a = platform.New(db, platform.Config{MasterKey: make([]byte, 32), NodeAllowCIDRs: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}})
	token, err := f.a.Encrypt([]byte(strings.Repeat("A", 43)))
	if err != nil {
		t.Fatal(err)
	}
	fingerprint := sha256.Sum256(server.Certificate().Raw)
	for _, statement := range []string{
		`INSERT INTO users(id,username,password_hash,role,status) VALUES(1,'plan-admin','test-hash','admin','active'),(2,'plan-owner','test-hash','user','active')`,
		`INSERT INTO user_limits(user_id,max_instances,max_vcpus,max_memory_bytes,max_disk_bytes,max_gpu_allocations,max_resident_instances) VALUES(2,20,100,1099511627776,1099511627776,8,20)`,
		`INSERT INTO pricing_versions(id,code,version,cpu_hourly_pt,memory_gib_hourly_pt,disk_gib_hourly_pt,status,effective_at,created_by_user_id) VALUES(1,'plan-price','1',1,0.5,0.01,'active',NOW(6)-INTERVAL 1 HOUR,1)`,
		`INSERT INTO credit_policy_versions(id,code,version,daily_allowance_pt,bonus_cap_pt,timezone,reset_time,billing_window_seconds,status,effective_at,created_by_user_id,daily_validity_seconds,shutdown_grace_seconds,grace_charge_mode,restore_action,storage_charge_mode,offline_charge_mode) VALUES(1,'plan-credit','1',100,10000,'Asia/Shanghai','00:00:00',60,'active',NOW(6)-INTERVAL 1 HOUR,1,86400,60,'continue','manual','retained','continue')`,
		`INSERT INTO idle_policy_versions(id,code,version,sample_interval_seconds,evaluation_window_seconds,consecutive_idle_windows,cpu_utilization_threshold_pct,gpu_utilization_threshold_pct,gpu_memory_threshold_pct,network_bytes_threshold,action,status,effective_at,created_by_user_id) VALUES(1,'plan-idle','1',10,60,2,5,5,5,0,'release_gpu','active',NOW(6)-INTERVAL 1 HOUR,1)`,
		`INSERT INTO gpu_price_versions(id,code,version,name,gpu_model,access_mode,gpu_hourly_pt,status,effective_at,created_by_user_id) VALUES(1,'plan-gpu','1','Plan GPU','NVIDIA A100','shared',2,'active',NOW(6)-INTERVAL 1 HOUR,1)`,
		`INSERT INTO policy_versions(id,kind,code,version,source_id,config,status,effective_at,created_by_user_id) VALUES
		(1,'pricing','plan-price','1',1,'{"cpu_hourly_pt":"1.000000","memory_gib_hourly_pt":"0.500000","disk_gib_hourly_pt":"0.010000"}','active',NOW(6)-INTERVAL 1 HOUR,1),
		(2,'credit','plan-credit','1',1,'{"billing_window_seconds":60,"storage_charge_mode":"retained","offline_charge_mode":"continue"}','active',NOW(6)-INTERVAL 1 HOUR,1),
		(3,'gpu-idle','plan-idle','1',1,'{"enabled":true,"default_timeout_seconds":1800,"min_timeout_seconds":60,"max_timeout_seconds":7200,"allowed_actions":["release_gpu","stop_instance"],"graceful_stop_timeout_seconds":60}','active',NOW(6)-INTERVAL 1 HOUR,1),
		(4,'gpu-scheduling','plan-scheduling','1',NULL,'{"metrics_max_age_seconds":60,"cooldown_seconds":600,"allow_price_increase":false}','active',NOW(6)-INTERVAL 1 HOUR,1),
		(5,'metrics','plan-metrics','1',NULL,'{"raw_retention_days":7,"rollup_retention_days":30,"bucket_seconds":60,"collection_interval_seconds":10}','active',NOW(6)-INTERVAL 1 HOUR,1),
		(6,'gpu-pricing','plan-gpu','1',1,'{"gpu_model":"NVIDIA A100","access_mode":"shared","gpu_hourly_pt":"2.000000"}','active',NOW(6)-INTERVAL 1 HOUR,1)`,
		`INSERT INTO credit_accounts(id,user_id,credit_policy_version_id,status) VALUES(1,2,1,'active')`,
		`INSERT INTO credit_buckets(account_id,source_type,business_key,granted_pt,remaining_pt,reserved_pt,valid_from) VALUES(1,'admin','plan-fixture',10000,10000,0,NOW(6)-INTERVAL 1 HOUR)`,
		`INSERT INTO images(id,code,version,name,source_type,fingerprint,architecture,image_type,gpu_ready,os_name,os_release,properties,status,created_by_user_id) VALUES(1,'plan-image','1','Plan image','local',UNHEX(SHA2('plan-image',256)),'x86_64','container',TRUE,'Linux','test',JSON_OBJECT(),'active',1)`,
	} {
		f.exec(statement)
	}
	f.exec(`INSERT INTO nodes(id,name,agent_endpoint,token_ciphertext,key_version,server_cert_sha256,default_storage_pool,default_project_name,allocatable_vcpus,allocatable_memory_bytes,allocatable_disk_bytes,resident_vcpu_budget,resident_memory_budget_bytes,resident_disk_budget_bytes,resources,resources_sampled_at,last_heartbeat_at,status,administrative_state,health,connection_verified_at,metrics_policy_id) VALUES(1,'plan-node',?,?,1,?,'default','default',100,1099511627776,1099511627776,100,1099511627776,1099511627776,'{"architecture":"x86_64"}',NOW(6),NOW(6),'active','active','online',NOW(6),5)`, server.URL, token, fingerprint[:])
	for _, statement := range []string{
		`INSERT INTO image_nodes(image_id,node_id,project_name,architecture,fingerprint,cloud_init,gpu_ready,properties,status,last_verified_at) VALUES(1,1,'default','x86_64',UNHEX(SHA2('plan-image',256)),TRUE,TRUE,JSON_OBJECT(),'ready',NOW(6))`,
		`INSERT INTO network_pools(id,name,address_family,network_address,prefix_length,range_start,range_end,dns_servers,status) VALUES(1,'plan-pool',4,INET6_ATON('192.0.2.0'),24,INET6_ATON('192.0.2.10'),INET6_ATON('192.0.2.20'),JSON_ARRAY(),'active')`,
		`INSERT INTO node_network_bindings(id,node_id,network_pool_id,mode,parent_interface,project_name,status,validation_status,validated_at) VALUES(1,1,1,'bridge','br0','default','active','verified',NOW(6))`,
		`INSERT INTO ip_addresses(network_pool_id,address_family,address,status) VALUES(1,4,INET6_ATON('192.0.2.10'),'available'),(1,4,INET6_ATON('192.0.2.11'),'available')`,
		`INSERT INTO instance_plans(id,code,version,name,image_type,vcpus,memory_bytes,disk_bytes,gpu_count,gpu_model,gpu_access_mode,gpu_max_hourly_pt,pricing_version_id,credit_policy_version_id,idle_policy_version_id,init_parameters,allow_residency,status,allowed_overrides,policy_versions,gpu_policy,allowed_user_actions,gpu_user_permissions) VALUES(1,'configured-plan','1','Configured plan','container',2,2147483648,1073741824,0,'NVIDIA A100','shared',3.25,1,1,1,JSON_OBJECT(),FALSE,'active','{"gpu_count":{"min":"0","max":"1","step":"1"}}','{"pricing":"1","credit":"2","gpu-idle":"3","gpu-scheduling":"4","metrics":"5","gpu-pricing":"6"}','{"idle":{"enabled":true,"timeout_seconds":900,"action":"detach_gpu"},"rebalance":{"enabled":true,"allow_live":true,"max_hourly_pt":"3.250000"}}','["stop","gpu_policy","console"]','{"idle":false,"idle_timeout":true,"idle_action":false,"rebalance":false,"allow_live":false,"max_hourly_pt":true}')`,
		`INSERT INTO plan_images(plan_id,image_id) VALUES(1,1)`,
		`INSERT INTO plan_network_options(plan_id,network_option_id) VALUES(1,1)`,
		`INSERT INTO plan_nodes(plan_id,node_id) VALUES(1,1)`,
	} {
		f.exec(statement)
	}
	f.router = gin.New()
	f.router.Use(platform.RequestMiddleware())
	user := f.router.Group("/api/v1", func(c *gin.Context) {
		id := f.requestUserID
		if id == 0 {
			id = 2
		}
		c.Set("user", platform.User{ID: id, Role: "user"})
		c.Next()
	}, f.a.Mutations())
	admin := f.router.Group("/api/v1/admin", func(c *gin.Context) { c.Set("user", platform.User{ID: 1, Role: "admin"}); c.Next() }, platform.Admin(), f.a.Mutations())
	Register(f.a, user, admin)
	catalog.Register(f.a, user, admin)
	return f
}

func (f *planConfigurationFixture) exec(statement string, args ...any) {
	f.t.Helper()
	if err := f.db.Exec(statement, args...).Error; err != nil {
		f.t.Fatal(err)
	}
}

func (f *planConfigurationFixture) call(admin bool, method, path string, body any, revision uint64, status int, code string) map[string]any {
	f.t.Helper()
	prefix := "/api/v1"
	if admin {
		prefix += "/admin"
	}
	f.requests++
	req := httptest.NewRequest(method, prefix+path, strings.NewReader(jsonText(body)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", fmt.Sprintf("plan-regression-%d", f.requests))
	if revision != 0 {
		req.Header.Set("If-Match", fmt.Sprintf(`"rev-%d"`, revision))
	}
	w := httptest.NewRecorder()
	f.router.ServeHTTP(w, req)
	if w.Code != status {
		f.t.Fatalf("%s %s: status=%d want=%d body=%s", method, path, w.Code, status, w.Body.String())
	}
	var envelope map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &envelope); err != nil {
		f.t.Fatalf("decode %s: %v", w.Body.String(), err)
	}
	if code != "" && platform.Text(object(envelope["error"])["code"]) != code {
		f.t.Fatalf("%s %s: expected %s: %s", method, path, code, w.Body.String())
	}
	return object(envelope["data"])
}

func (f *planConfigurationFixture) createBody(node, cap string) platform.Row {
	return platform.Row{"name": "elastic-plan-instance", "plan_id": "1", "plan_revision": "1", "image_id": "1", "network_option_id": "1", "node_id": node, "resources": Resources{VCPUs: 2, MemoryBytes: "2147483648", DiskBytes: "1073741824"}, "gpu": catalog.GPURequest{Count: 0, AccessMode: "shared", MaxHourlyPT: cap}, "init_parameters": platform.Row{}, "policy_versions": map[string]string{"pricing": "1", "credit": "2", "gpu-idle": "3", "gpu-scheduling": "4", "metrics": "5", "gpu-pricing": "6"}}
}

func (f *planConfigurationFixture) instance(id uint64) platform.Row {
	f.t.Helper()
	r, err := platform.Get(f.db, "instances", id, false)
	if err != nil {
		f.t.Fatal(err)
	}
	return r
}

func (f *planConfigurationFixture) count(table string) int64 {
	f.t.Helper()
	var n int64
	if err := f.db.Table(table).Count(&n).Error; err != nil {
		f.t.Fatal(err)
	}
	return n
}

func planConfigurationSnapshot(r platform.Row) string {
	return jsonText(platform.Row{"gpu_policy": object(r["gpu_policy"]), "allowed_user_actions": rows(r["allowed_user_actions"]), "gpu_user_permissions": object(r["gpu_user_permissions"]), "policy_versions": object(r["policy_versions"]), "max_gpu_hourly_pt": platform.Text(r["max_gpu_hourly_pt"]), "auto_release_gpu": platform.Bool(r["auto_release_gpu"]), "auto_rebind_gpu": platform.Bool(r["auto_rebind_gpu"])})
}

func TestMySQLElasticCreateSnapshotsPlanConfiguration(t *testing.T) {
	f := newPlanConfigurationFixture(t)
	for _, tc := range []struct{ name, node, cap string }{
		{"node_network_mismatch", "2", "2.5"},
		{"cap_above_plan", "1", "3.250001"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f.call(false, http.MethodPost, "/instances", f.createBody(tc.node, tc.cap), 0, http.StatusUnprocessableEntity, "validation_failed")
			for _, table := range []string{"instances", "tasks", "usage_sessions", "credit_reservations"} {
				if n := f.count(table); n != 0 {
					t.Fatalf("rejected create leaked %s: %d", table, n)
				}
			}
			if f.reads.Load() != 0 {
				t.Fatal("invalid placement/cap reached native admission")
			}
		})
	}
	// The plan also has a network relation: dropping only its node allowlist
	// must still reject admission, even when node_id matches that network.
	f.exec("DELETE FROM plan_nodes WHERE plan_id=1")
	f.call(false, http.MethodPost, "/instances", f.createBody("1", "2.5"), 0, http.StatusUnprocessableEntity, "validation_failed")
	if f.count("instances") != 0 || f.count("tasks") != 0 || f.reads.Load() != 0 {
		t.Fatal("missing plan node permission admitted or dispatched create")
	}
	f.exec("INSERT INTO plan_nodes(plan_id,node_id) VALUES(1,1)")
	created := f.call(false, http.MethodPost, "/instances", f.createBody("1", "2.5"), 0, http.StatusAccepted, "")
	id := platform.Uint(created["instance_id"])
	if id == 0 {
		t.Fatalf("create did not identify reserved instance: %+v", created)
	}
	r := f.instance(id)
	wantPolicy := `{"idle":{"enabled":true,"timeout_seconds":900,"action":"detach_gpu"},"rebalance":{"enabled":true,"allow_live":true,"max_hourly_pt":"2.500000"}}`
	if !reflect.DeepEqual(object(r["gpu_policy"]), object(wantPolicy)) || platform.Text(r["max_gpu_hourly_pt"]) != "2.500000" || !platform.Bool(r["auto_release_gpu"]) || !platform.Bool(r["auto_rebind_gpu"]) {
		t.Fatalf("plan policy/lower cap not snapshotted consistently: %+v", r)
	}
	if !reflect.DeepEqual(rows(r["allowed_user_actions"]), rows(`["stop","gpu_policy","console"]`)) || !reflect.DeepEqual(object(r["gpu_user_permissions"]), object(`{"idle":false,"idle_timeout":true,"idle_action":false,"rebalance":false,"allow_live":false,"max_hourly_pt":true}`)) {
		t.Fatalf("plan security grants not snapshotted: %+v", r)
	}
	if platform.Text(r["product_type"]) != "elastic" || platform.Uint(r["instance_plan_id"]) != 1 || platform.Uint(r["node_id"]) != 1 || f.count("usage_sessions") != 2 || f.count("credit_reservations") != 2 || f.reads.Load() != 1 {
		t.Fatalf("create bypassed real resource/financial admission: %+v", r)
	}
	var reserved int64
	if err := f.db.Table("ip_addresses").Where("instance_id=? AND status='reserved' AND reservation_task_id=?", id, created["task_id"]).Count(&reserved).Error; err != nil || reserved != 1 {
		t.Fatalf("create did not bind its IP/task reservation: %d %v", reserved, err)
	}
	snapshot := planConfigurationSnapshot(r)
	// Editing and disabling the catalog must not rewrite an existing guest.
	f.exec(`UPDATE instance_plans SET gpu_policy='{"idle":{"enabled":false,"timeout_seconds":1800,"action":"stop_instance"},"rebalance":{"enabled":false,"allow_live":false,"max_hourly_pt":"1.000000"}}',gpu_max_hourly_pt=1,allowed_user_actions='["restart"]',gpu_user_permissions='{"idle":true}',revision=revision+1 WHERE id=1`)
	if got := planConfigurationSnapshot(f.instance(id)); got != snapshot {
		t.Fatalf("future plan edit rewrote instance snapshot: %s", got)
	}
	f.exec("UPDATE instance_plans SET status='disabled',revision=revision+1 WHERE id=1")
	if got := planConfigurationSnapshot(f.instance(id)); got != snapshot {
		t.Fatalf("plan disable rewrote instance snapshot: %s", got)
	}
	policy := f.call(false, http.MethodGet, fmt.Sprintf("/instances/%d/gpu-policy", id), platform.Row{}, 0, http.StatusOK, "")
	if !reflect.DeepEqual(object(policy["idle"]), object(wantPolicy)["idle"]) || !reflect.DeepEqual(object(policy["rebalance"]), object(wantPolicy)["rebalance"]) {
		t.Fatalf("disabled plan leaked into policy read: %+v", policy)
	}
}

func TestMySQLElasticPlanSecurityGrantsEnforceRealMutations(t *testing.T) {
	f := newPlanConfigurationFixture(t)
	created := f.call(false, http.MethodPost, "/instances", f.createBody("1", "2.5"), 0, http.StatusAccepted, "")
	id := platform.Uint(created["instance_id"])
	path := "/instances/" + strconv.FormatUint(id, 10)
	// Seed completion evidence only after exercising actual create admission;
	// worker/native lifecycle execution is outside this admission regression.
	f.exec("UPDATE tasks SET status='succeeded',finished_at=NOW(6) WHERE id=?", created["task_id"])
	f.exec("UPDATE instances SET observed_state='running',provisioning_state='ready',observed_at=NOW(6) WHERE id=?", id)
	r := f.instance(id)
	initial := planConfigurationSnapshot(r)
	// Existing guests must continue enforcing their own grants even if catalog
	// contents later disagree and the originating plan is no longer available.
	f.exec(`UPDATE instance_plans SET status='disabled',allowed_user_actions='["restart"]',gpu_user_permissions='{"idle":true,"idle_timeout":false,"idle_action":true,"rebalance":true,"allow_live":true,"max_hourly_pt":false}',revision=revision+1 WHERE id=1`)
	revision := platform.Uint(r["revision"])
	beforeTasks := f.count("tasks")
	f.call(false, http.MethodPost, path+"/restart", platform.Row{}, revision, http.StatusForbidden, "action_not_allowed")
	if f.count("tasks") != beforeTasks || platform.Uint(f.instance(id)["revision"]) != revision {
		t.Fatal("denied elastic lifecycle action changed state or queued native work")
	}
	view := f.call(false, http.MethodGet, path, platform.Row{}, 0, http.StatusOK, "")
	if platform.Bool(object(view["capabilities"])["gpu_attach"]) || platform.Bool(object(view["capabilities"])["gpu_rebind"]) || !platform.Bool(object(view["capabilities"])["stop"]) || !platform.Bool(object(view["capabilities"])["console"]) {
		t.Fatalf("instance capabilities ignore plan action grants: %+v", view)
	}
	base := instanceGPUPolicy{Idle: storedIdlePolicy{Enabled: true, TimeoutSeconds: 900, Action: "detach_gpu"}, Rebalance: storedRebalancePolicy{Enabled: true, AllowLive: true, MaxHourlyPT: "2.500000"}}
	for _, tc := range []struct {
		name   string
		change func(*instanceGPUPolicy)
	}{
		{"idle", func(p *instanceGPUPolicy) { p.Idle.Enabled = false }},
		{"idle_action", func(p *instanceGPUPolicy) { p.Idle.Action = "stop_instance" }},
		{"rebalance", func(p *instanceGPUPolicy) { p.Rebalance.Enabled = false }},
		{"allow_live", func(p *instanceGPUPolicy) { p.Rebalance.AllowLive = false }},
	} {
		t.Run("denied_"+tc.name, func(t *testing.T) {
			cfg := base
			tc.change(&cfg)
			f.call(false, http.MethodPut, path+"/gpu-policy", cfg, revision, http.StatusForbidden, "gpu_policy_not_authorized")
			if current := f.instance(id); planConfigurationSnapshot(current) != initial || platform.Uint(current["revision"]) != revision || f.count("tasks") != beforeTasks {
				t.Fatalf("rejected %s changed snapshot/revision/tasks: %+v", tc.name, current)
			}
		})
	}
	policy := f.call(false, http.MethodGet, path+"/gpu-policy", platform.Row{}, 0, http.StatusOK, "")
	editable := object(object(policy["allowed_ranges"])["editable"])
	for _, key := range []string{"idle", "idle_timeout", "idle_action", "rebalance", "allow_live", "max_hourly_pt"} {
		want := key == "idle_timeout" || key == "max_hourly_pt"
		if platform.Bool(editable[key]) != want {
			t.Fatalf("policy DTO misreports %s permission: %+v", key, policy)
		}
	}
	allowed := base
	allowed.Idle.TimeoutSeconds = 1200
	allowed.Rebalance.MaxHourlyPT = "2.25"
	f.call(false, http.MethodPut, path+"/gpu-policy", allowed, revision, http.StatusOK, "")
	r = f.instance(id)
	if platform.Uint(r["revision"]) != revision+1 || platform.Uint(object(object(r["gpu_policy"])["idle"])["timeout_seconds"]) != 1200 || platform.Text(r["max_gpu_hourly_pt"]) != "2.250000" || platform.Text(object(object(r["gpu_policy"])["rebalance"])["max_hourly_pt"]) != "2.250000" {
		t.Fatalf("authorized elastic policy fields did not persist: %+v", r)
	}
	adminPolicy := allowed
	adminPolicy.Idle.Enabled = false
	adminPolicy.Idle.Action = "stop_instance"
	adminPolicy.Rebalance.Enabled = false
	adminPolicy.Rebalance.AllowLive = false
	f.call(true, http.MethodPut, path+"/gpu-policy", adminPolicy, platform.Uint(r["revision"]), http.StatusOK, "")
	r = f.instance(id)
	if platform.Bool(r["auto_release_gpu"]) || platform.Bool(r["auto_rebind_gpu"]) || platform.Text(object(object(r["gpu_policy"])["idle"])["action"]) != "stop_instance" {
		t.Fatalf("admin could not bypass field-level grants: %+v", r)
	}
	// Admin bypass must not silently grant the user the denied lifecycle action.
	if !reflect.DeepEqual(rows(r["allowed_user_actions"]), rows(`["stop","gpu_policy","console"]`)) {
		t.Fatalf("admin policy edit escalated user action grants: %+v", r)
	}
	f.call(true, http.MethodPost, path+"/restart", platform.Row{}, platform.Uint(r["revision"]), http.StatusAccepted, "")
	var restarted int64
	if err := f.db.Table("tasks").Where("instance_id=? AND task_type='instance.restart' AND status='queued'", id).Count(&restarted).Error; err != nil || restarted != 1 {
		t.Fatalf("admin bypass did not queue actual restart: %d %v", restarted, err)
	}
	f.exec("UPDATE tasks SET status='succeeded',finished_at=NOW(6) WHERE instance_id=? AND status='queued'", id)
	r = f.instance(id)
	f.call(false, http.MethodPost, path+"/stop", platform.Row{}, platform.Uint(r["revision"]), http.StatusAccepted, "")
	var stopped int64
	if err := f.db.Table("tasks").Where("instance_id=? AND task_type='instance.stop' AND status='queued'", id).Count(&stopped).Error; err != nil || stopped != 1 || platform.Text(f.instance(id)["desired_state"]) != "stopped" {
		t.Fatalf("authorized user stop did not queue/persist intent: %d %v", stopped, err)
	}
	if f.reads.Load() != 1 {
		t.Fatal("lifecycle/policy admission unexpectedly dispatched native operations")
	}
}

func TestMySQLEditablePlanInvalidatesQuotesAndFreezesResizeBounds(t *testing.T) {
	f := newPlanConfigurationFixture(t)
	f.exec(`UPDATE instance_plans SET gpu_model=NULL,allowed_overrides='{"vcpus":{"min":"2","max":"4","step":"1"}}',allowed_user_actions='["start","stop","resize","console"]' WHERE id=1`)
	quoteRequest := func(body platform.Row) platform.Row {
		out := platform.Row{}
		for k, v := range body {
			out[k] = v
		}
		delete(out, "policy_versions")
		out["gpu"] = catalog.GPURequest{Count: 0, AccessMode: "shared", MaxHourlyPT: "2.5"}
		return out
	}
	body := f.createBody("1", "2.5")
	quote := f.call(false, http.MethodPost, "/quotes", quoteRequest(body), 0, http.StatusOK, "")
	if platform.Text(quote["plan_revision"]) != "1" {
		t.Fatalf("quote did not identify its mutable plan revision: %+v", quote)
	}
	missingRevision := f.createBody("1", "2.5")
	delete(missingRevision, "plan_revision")
	f.call(false, http.MethodPost, "/instances", missingRevision, 0, http.StatusConflict, "plan_changed")
	created := f.call(false, http.MethodPost, "/instances", body, 0, http.StatusAccepted, "")
	id := platform.Uint(created["instance_id"])
	old := f.instance(id)
	oldSnapshot := jsonText(object(old["plan_snapshot"]))
	updated := f.call(true, http.MethodPatch, "/plans/1", platform.Row{
		"name": "Updated in place", "resources": Resources{VCPUs: 4, MemoryBytes: "2147483648", DiskBytes: "1073741824"},
		"allowed_overrides": platform.Row{"vcpus": platform.Row{"min": "4", "max": "8", "step": "1"}},
	}, 1, http.StatusOK, "")
	if platform.Text(updated["status"]) != "active" || platform.Text(updated["revision"]) != "2" {
		t.Fatalf("active edit did not remain independently enabled: %+v", updated)
	}
	f.call(false, http.MethodPost, "/instances", body, 0, http.StatusConflict, "plan_changed")
	if f.count("instances") != 1 || f.reads.Load() != 1 {
		t.Fatal("stale quotation created or dispatched additional resources")
	}
	fresh := f.createBody("1", "2.5")
	delete(fresh, "plan_revision")
	fresh["resources"] = Resources{VCPUs: 4, MemoryBytes: "2147483648", DiskBytes: "1073741824"}
	newQuote := f.call(false, http.MethodPost, "/quotes", quoteRequest(fresh), 0, http.StatusOK, "")
	fresh["plan_revision"] = newQuote["plan_revision"]
	fresh["policy_versions"] = newQuote["policy_versions"]
	newCreated := f.call(false, http.MethodPost, "/instances", fresh, 0, http.StatusAccepted, "")
	newGuest := f.instance(platform.Uint(newCreated["instance_id"]))
	if platform.Uint(newGuest["vcpus"]) != 4 || platform.Uint(object(newGuest["plan_snapshot"])["vcpus"]) != 4 || platform.Text(object(newGuest["plan_snapshot"])["revision"]) != "2" {
		t.Fatalf("fresh creation did not capture the edited offering: %+v", newGuest)
	}
	if platform.Uint(f.instance(id)["vcpus"]) != 2 || jsonText(object(f.instance(id)["plan_snapshot"])) != oldSnapshot {
		t.Fatal("plan edit changed an existing guest or its resize contract")
	}
	f.call(true, http.MethodPost, "/plans/1/disable", platform.Row{}, 2, http.StatusOK, "")
	f.exec("UPDATE tasks SET status='succeeded',finished_at=NOW(6) WHERE instance_id=?", id)
	f.exec("UPDATE instances SET observed_state='stopped',desired_state='stopped',provisioning_state='ready',observed_at=NOW(6) WHERE id=?", id)
	path := fmt.Sprintf("/instances/%d/resize", id)
	beforeTasks := f.count("tasks")
	f.call(false, http.MethodPost, path, platform.Row{"resources": Resources{VCPUs: 5, MemoryBytes: "2147483648", DiskBytes: "1073741824"}}, platform.Uint(old["revision"]), http.StatusUnprocessableEntity, "invalid_request")
	if f.count("tasks") != beforeTasks || platform.Uint(f.instance(id)["vcpus"]) != 2 {
		t.Fatal("edited plan expanded an old instance's resize authorization")
	}
	f.call(false, http.MethodPost, path, platform.Row{"resources": Resources{VCPUs: 3, MemoryBytes: "2147483648", DiskBytes: "1073741824"}}, platform.Uint(old["revision"]), http.StatusAccepted, "")
	if platform.Uint(f.instance(id)["vcpus"]) != 3 || jsonText(object(f.instance(id)["plan_snapshot"])) != oldSnapshot {
		t.Fatal("disabled/edited plan blocked the original resize contract or replaced its snapshot")
	}
}

func TestMySQLQuoteDoesNotRequireCreationName(t *testing.T) {
	f := newPlanConfigurationFixture(t)
	for _, tc := range []struct {
		label   string
		name    string
		present bool
	}{
		{label: "missing"},
		{label: "empty", present: true},
		{label: "whitespace", name: "   ", present: true},
		{label: "too_long", name: strings.Repeat("x", 129), present: true},
	} {
		t.Run(tc.label, func(t *testing.T) {
			body := f.createBody("1", "2.5")
			delete(body, "name")
			delete(body, "policy_versions")
			body["gpu"] = catalog.GPURequest{Count: 0, AccessMode: "shared", MaxHourlyPT: "2.5"}
			if tc.present {
				body["name"] = tc.name
			}
			quote := f.call(false, http.MethodPost, "/quotes", body, 0, http.StatusOK, "")
			if !platform.Bool(quote["admissible"]) || platform.Text(object(quote["price_details"])["total_hourly_pt"]) != "3.010000" {
				t.Fatalf("name affected billable configuration: %+v", quote)
			}
			body["plan_revision"] = quote["plan_revision"]
			body["policy_versions"] = quote["policy_versions"]
			f.call(false, http.MethodPost, "/instances", body, 0, http.StatusUnprocessableEntity, "invalid_request")
		})
	}
	for _, table := range []string{"instances", "tasks", "usage_sessions", "credit_reservations"} {
		if f.count(table) != 0 {
			t.Fatalf("unnamed quote or rejected creation changed %s", table)
		}
	}
	if f.reads.Load() != 0 {
		t.Fatal("unnamed quote or rejected creation reached native admission")
	}
	body := f.createBody("1", "2.5")
	body["name"] = "name-entered-after-quote"
	created := f.call(false, http.MethodPost, "/instances", body, 0, http.StatusAccepted, "")
	if platform.Text(f.instance(platform.Uint(created["instance_id"]))["logical_name"]) != body["name"] {
		t.Fatal("creation did not preserve the subsequently entered name")
	}
}

func TestMySQLTwoPlansFreezeDistinctPricesOnSameGPU(t *testing.T) {
	f := newPlanConfigurationFixture(t)
	f.exec(`UPDATE policy_versions SET config=JSON_SET(config,'$.sm_weight',0.5,'$.memory_weight',0.5) WHERE id=4`)
	f.exec(`UPDATE nodes SET resources=JSON_OBJECT('architecture','x86_64','gpus',JSON_ARRAY(JSON_OBJECT('uuid','GPU-plan','utilization_percent',0,'memory_utilization_percent',0))),resources_sampled_at=NOW(6) WHERE id=1`)
	f.exec(`INSERT INTO node_gpus(node_id,uuid,model,memory_mib,max_allocations,status,last_seen_at) VALUES(1,'GPU-plan','NVIDIA A100',81920,8,'available',NOW(6))`)
	var plan platform.Row
	if err := f.db.Table("instance_plans").Where("id=1").Take(&plan).Error; err != nil {
		t.Fatal(err)
	}
	plan["id"], plan["code"], plan["gpu_max_hourly_pt"] = 2, "second-plan", "10.000000"
	if err := f.db.Table("instance_plans").Create(&plan).Error; err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"plan_images", "plan_nodes", "plan_network_options"} {
		column := map[string]string{"plan_images": "image_id", "plan_nodes": "node_id", "plan_network_options": "network_option_id"}[table]
		f.exec("INSERT INTO " + table + "(plan_id," + column + ") VALUES(2,1)")
	}
	for i, rate := range []string{"4.000000", "7.000000"} {
		id := i + 2
		profile := i + 7
		f.exec(`INSERT INTO gpu_price_versions(id,code,version,name,gpu_model,access_mode,gpu_hourly_pt,status,effective_at,created_by_user_id) VALUES(?,?,'1','Plan GPU rate','NVIDIA A100','shared',?,'active',NOW(6)-INTERVAL 1 HOUR,1)`, id, fmt.Sprintf("gpu-rate-%d", id), rate)
		f.exec(`INSERT INTO policy_versions(id,kind,code,version,source_id,config,status,effective_at,created_by_user_id) VALUES(?,'gpu-pricing',?,'1',?,JSON_OBJECT('gpu_model','NVIDIA A100','access_mode','shared','gpu_hourly_pt',?),'active',NOW(6)-INTERVAL 1 HOUR,1)`, profile, fmt.Sprintf("gpu-rate-%d", id), id, rate)
		f.exec(`UPDATE instance_plans SET gpu_max_hourly_pt=10,policy_versions=JSON_SET(policy_versions,'$."gpu-pricing"',?) WHERE id=?`, strconv.Itoa(profile), i+1)
		body := f.createBody("1", "10")
		body["plan_id"] = strconv.Itoa(i + 1)
		body["gpu"] = GPURequest{Count: 1, Model: "NVIDIA A100", AccessMode: "shared", MaxHourlyPT: "10", MinMemoryMiB: "0"}
		versions := body["policy_versions"].(map[string]string)
		versions["gpu-pricing"] = strconv.Itoa(profile)
		quoteBody := f.createBody("1", "10")
		quoteBody["plan_id"] = body["plan_id"]
		model := "NVIDIA A100"
		quoteBody["gpu"] = catalog.GPURequest{Count: 1, Model: &model, AccessMode: "shared", MaxHourlyPT: "10"}
		delete(quoteBody, "policy_versions")
		quote := f.call(false, http.MethodPost, "/quotes", quoteBody, 0, http.StatusOK, "")
		if platform.Text(object(quote["price_details"])["gpu_hourly_pt"]) != rate || !platform.Bool(quote["admissible"]) {
			t.Fatalf("plan quote does not match its GPU profile: %+v", quote)
		}
		created := f.call(false, http.MethodPost, "/instances", body, 0, http.StatusAccepted, "")
		r := f.instance(platform.Uint(created["instance_id"]))
		var allocation platform.Row
		if err := f.db.Table("gpu_allocations").Where("instance_id=?", r["id"]).Take(&allocation).Error; err != nil {
			t.Fatal(err)
		}
		if platform.Text(allocation["gpu_hourly_pt"]) != rate || platform.Uint(allocation["gpu_price_version_id"]) != uint64(id) || platform.Uint(allocation["gpu_id"]) != 1 {
			t.Fatalf("plan %d allocation did not retain its bound price: %+v", i+1, allocation)
		}
	}
}

func TestMySQLLegacyCustomGPUPriceRequiresExplicitOneTimeConfiguration(t *testing.T) {
	f := newPlanConfigurationFixture(t)
	created := f.call(false, http.MethodPost, "/instances", f.createBody("1", "2.5"), 0, http.StatusAccepted, "")
	id := platform.Uint(created["instance_id"])
	f.exec(`UPDATE instances SET product_type='custom',billing_mode='admin_exempt',instance_plan_id=NULL,plan_snapshot=NULL,policy_versions=JSON_REMOVE(policy_versions,'$."gpu-pricing"') WHERE id=?`, id)
	r := f.instance(id)
	if _, err := f.a.InstancePolicy(t.Context(), r, "gpu-pricing"); err == nil {
		t.Fatal("legacy missing binding selected an implicit price")
	}
	path := fmt.Sprintf("/instances/%d/gpu-pricing-policy", id)
	input := platform.Row{"policy_version_id": "6"}
	f.call(true, http.MethodPut, path, input, platform.Uint(r["revision"]), http.StatusOK, "")
	r = f.instance(id)
	if platform.Text(object(r["policy_versions"])["gpu-pricing"]) != "6" {
		t.Fatal("explicit legacy price binding was not persisted")
	}
	f.call(true, http.MethodPut, path, input, platform.Uint(r["revision"]), http.StatusConflict, "policy_binding_frozen")
}

func TestMySQLGPUProfileCatalogIsAdminOnly(t *testing.T) {
	f := newPlanConfigurationFixture(t)
	userRequest := httptest.NewRequest(http.MethodGet, "/api/v1/catalog/gpu-options", nil)
	response := httptest.NewRecorder()
	f.router.ServeHTTP(response, userRequest)
	if response.Code != http.StatusNotFound {
		t.Fatalf("member GPU profile catalog is still exposed: %d", response.Code)
	}
	deniedRouter := gin.New()
	member := deniedRouter.Group("/api/v1", func(c *gin.Context) {
		c.Set("user", platform.User{ID: 2, Role: "user"})
		c.Next()
	})
	catalog.Register(platform.New(f.db, f.a.Config), member, member.Group("/admin", platform.Admin()))
	denied := httptest.NewRecorder()
	deniedRouter.ServeHTTP(denied, httptest.NewRequest(http.MethodGet, "/api/v1/admin/catalog/gpu-options", nil))
	if denied.Code != http.StatusForbidden {
		t.Fatalf("member reached administrator GPU profile catalog: %d", denied.Code)
	}
	f.call(true, http.MethodGet, "/catalog/gpu-options", nil, 0, http.StatusOK, "")
}
