package billing

import (
	"encoding/json"
	"net/http"
	"reflect"
	"testing"
	"time"

	"101cloud/backend/internal/platform"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"
)

func TestMySQLPendingDebtBlocksAllNewSpending(t *testing.T) {
	f := newAdjustmentMySQLFixture(t)
	f.addDebt(t, []string{"3.000003"})
	result := adjustmentResponse(t, f.call(http.MethodPost, `{"amount_pt":"2.000002"}`, "pending-funds", f.etag(t)))
	f.assertFresh(t, result)
	beforeLedger, beforeRevision := f.count(t, "credit_ledger", "1=1"), f.etag(t)
	response := f.call(http.MethodPost, `{"amount_pt":"-0.000001"}`, "steal-debt", beforeRevision)
	if response.Code != http.StatusConflict {
		t.Fatalf("negative adjustment stole debt funding: %d %s", response.Code, response.Body.String())
	}
	for _, action := range []string{"create", "start", "restart", "resize", "gpu_attach"} {
		err := f.db.Transaction(func(db *gorm.DB) error {
			instance, err := platform.Get(db, "instances", 1, false)
			if err != nil {
				return err
			}
			instance["additional_gpu_hourly_pt"] = "1.000000"
			return AdmitInstance(f.ctx, db, instance, action)
		})
		if err == nil {
			t.Fatalf("%s spent funds owed to old debt", action)
		}
	}
	for _, kind := range []string{"base_compute", "storage", "gpu"} {
		err := f.db.Transaction(func(db *gorm.DB) error {
			ac, err := platform.Get(db, "credit_accounts", 1, true)
			if err != nil {
				return err
			}
			p, err := sessionCreditPolicy(db, debtRow(t, db, "usage_sessions", 2))
			if err != nil {
				return err
			}
			s := debtRow(t, db, "usage_sessions", 2)
			s["charge_kind"] = kind
			s["config_snapshot"] = `{"vcpus":1,"memory_bytes":"1073741824","disk_bytes":"1073741824"}`
			s["cpu_hourly_pt"], s["disk_gib_hourly_pt"], s["gpu_hourly_pt"] = "1.000000", "1.000000", "1.000000"
			return reserveWindow(db, ac, p, s, now(), true)
		})
		if err == nil {
			t.Fatalf("%s reservation spent pending debt funds", kind)
		}
	}
	if f.etag(t) != beforeRevision || f.count(t, "credit_ledger", "1=1") != beforeLedger || f.count(t, "backend_requests", "1=1") != 1 {
		t.Fatal("rejected spending left partial account/ledger/idempotency writes")
	}
	// A newer billed interval may establish debt, but cannot consume the grant.
	if err := f.db.Transaction(func(db *gorm.DB) error {
		ac, err := platform.Get(db, "credit_accounts", 1, true)
		if err != nil {
			return err
		}
		at := now()
		charge, err := insert(db, "usage_charges", row{"account_id": 1, "session_id": 2, "from_at": at.Add(-time.Second), "to_at": at, "expected_pt": "0.000001", "settled_pt": "0.000000", "status": "pending", "calculation_snapshot": `{}`})
		if err != nil {
			return err
		}
		return payCharge(db, ac, row{"id": 2}, charge, at)
	}); err != nil {
		t.Fatal(err)
	}
	if f.count(t, "credit_ledger", "entry_type='settle'") != 0 || f.count(t, "usage_charges", "id=2 AND status='insufficient' AND settled_pt=0") != 1 {
		t.Fatal("new usage jumped ahead of historical debt")
	}
}

func TestMySQLGrantCommitsBetweenConcurrentWorkerChunks(t *testing.T) {
	f := newAdjustmentMySQLFixture(t)
	amounts := make([]string, 6001)
	for i := range amounts {
		amounts[i] = "0.000002"
	}
	f.addDebt(t, amounts)
	adjustmentResponse(t, f.call(http.MethodPost, `{"amount_pt":"0.012002"}`, "worker-funds", f.etag(t)))
	ready := make(chan string, 1)
	release := make(chan struct{})
	workerDone := make(chan error, 1)
	go func() {
		workerDone <- f.db.Transaction(func(db *gorm.DB) error {
			ac, err := platform.Get(db, "credit_accounts", 1, true)
			if err != nil {
				return err
			}
			if err = restore(db, ac, now()); err != nil {
				return err
			}
			fresh, err := platform.Get(db, "credit_accounts", 1, false)
			if err != nil {
				return err
			}
			ready <- `"rev-` + sid(fresh["revision"]) + `"`
			<-release
			return nil
		})
	}()
	var etag string
	select {
	case etag = <-ready:
	case err := <-workerDone:
		t.Fatalf("worker failed before chunk boundary: %v", err)
	}
	requestStarted := make(chan struct{})
	requestDone := make(chan *http.Response, 1)
	// Recorder results are delivered separately; no testing API runs in goroutines.
	resultDone := make(chan map[string]any, 1)
	errDone := make(chan error, 1)
	go func() {
		close(requestStarted)
		r := f.call(http.MethodPost, `{"amount_pt":"0.000001"}`, "concurrent-grant", etag)
		requestDone <- r.Result()
		var envelope struct {
			Data map[string]any `json:"data"`
		}
		if err := json.Unmarshal(r.Body.Bytes(), &envelope); err != nil {
			errDone <- err
			return
		}
		resultDone <- envelope.Data
	}()
	<-requestStarted
	close(release)
	if err := <-workerDone; err != nil {
		t.Fatal(err)
	}
	if r := <-requestDone; r.StatusCode != http.StatusCreated {
		t.Fatalf("concurrent grant status=%d", r.StatusCode)
	}
	select {
	case result := <-resultDone:
		assertAdjustmentFields(t, result, map[string]any{"settled_debt_pt": "0.000000", "settlement_pending": true, "available_pt": "0.000001"})
	case err := <-errDone:
		t.Fatal(err)
	}
	if got := f.count(t, "usage_charges", "status='settled'"); got != settlementChunkSize {
		t.Fatalf("request waited for/drained %d historical charges", got)
	}
}

func TestMySQLQueuedGrantExpiryAndHeldReservationOwnership(t *testing.T) {
	f := newAdjustmentMySQLFixture(t)
	f.addDebt(t, []string{"3.000000", "2.000000"})
	if err := f.db.Exec("UPDATE usage_charges SET session_id=2 WHERE id=2").Error; err != nil {
		t.Fatal(err)
	}
	expiry := now().Add(time.Hour)
	grantResult := adjustmentResponse(t, f.call(http.MethodPost, `{"amount_pt":"5.000000","expires_at":"`+expiry.Format(time.RFC3339Nano)+`"}`, "expiring-pending", f.etag(t)))
	if err := f.db.Transaction(func(db *gorm.DB) error {
		ac, err := platform.Get(db, "credit_accounts", 1, true)
		if err != nil {
			return err
		}
		b, err := platform.Get(db, "credit_buckets", platform.Uint(grantResult["bucket_id"]), true)
		if err != nil {
			return err
		}
		r, err := insert(db, "credit_reservations", row{"reservation_key": "expired-own-held", "account_id": 1, "session_id": 2, "bucket_id": b["id"], "window_from": now(), "window_to": expiry, "reserved_at": now(), "expires_at": expiry, "reserved_pt": "3.000000", "settled_pt": "0.000000", "released_pt": "0.000000", "status": "held"})
		if err != nil {
			return err
		}
		if err = mutateBucket(db, b, "expired-own-held", "reserve", decimal.Zero, dec("3.000000"), row{"reservation_id": r["id"]}); err != nil {
			return err
		}
		future := expiry.Add(2 * time.Hour)
		if _, err = grant(db, ac, "admin", "future-funds", dec("9.000000"), future, nil, 1); err != nil {
			return err
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	first := debtRow(t, f.db, "usage_charges", 1)
	if err := f.db.Transaction(func(db *gorm.DB) error {
		ac, err := platform.Get(db, "credit_accounts", 1, true)
		if err != nil {
			return err
		}
		return restore(db, ac, expiry.Add(time.Second))
	}); err != nil {
		t.Fatal(err)
	}
	assertDebtUnchanged(t, f.db, first)
	if f.count(t, "usage_charges", "id=2 AND status='settled' AND settled_pt=2") != 1 {
		t.Fatal("expired owner-held funds were not usable by their session")
	}
	b := debtRow(t, f.db, "credit_buckets", platform.Uint(grantResult["bucket_id"]))
	if money(dec(b["remaining_pt"])) != "3.000000" || money(dec(b["reserved_pt"])) != "1.000000" {
		t.Fatalf("expired unreserved credit spent or reservation ownership lost: %#v", b)
	}
	if f.count(t, "credit_ledger", "entry_type='settle' AND charge_id=1") != 0 || f.count(t, "credit_buckets", "business_key='future-funds' AND remaining_pt=9") != 1 {
		t.Fatal("worker spent expired unreserved or future credit")
	}
	before := debtRow(t, f.db, "usage_charges", 2)
	if err := f.db.Transaction(func(db *gorm.DB) error {
		ac, err := platform.Get(db, "credit_accounts", 1, true)
		if err != nil {
			return err
		}
		return restore(db, ac, expiry.Add(2*time.Second))
	}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, debtRow(t, f.db, "usage_charges", 2)) {
		t.Fatal("retry duplicated owner-held payment")
	}
	if platform.Bool(debtRow(t, f.db, "credit_accounts", 1)["settlement_pending"]) {
		t.Fatal("unpayable debt stayed queued")
	}
}

func TestMySQLSettlementSourceFanoutIsBoundedAndRetryable(t *testing.T) {
	f := newAdjustmentMySQLFixture(t)
	f.addDebt(t, []string{"0.000065"})
	if err := f.db.Transaction(func(db *gorm.DB) error {
		ac, err := platform.Get(db, "credit_accounts", 1, true)
		if err != nil {
			return err
		}
		for i := range 65 {
			if _, err = grant(db, ac, "admin", "source-"+sid(i+1), dec("0.000001"), now(), nil, 1); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for pass, want := range []string{"0.000032", "0.000064", "0.000065"} {
		before := f.count(t, "credit_ledger", "entry_type='settle'")
		if err := settlePending(f.ctx, platform.New(f.db, platform.Config{})); err != nil {
			t.Fatal(err)
		}
		if delta := f.count(t, "credit_ledger", "entry_type='settle'") - before; delta > settlementChunkSize {
			t.Fatalf("pass %d touched %d payment buckets", pass, delta)
		}
		if got := money(dec(debtRow(t, f.db, "usage_charges", 1)["settled_pt"])); got != want {
			t.Fatalf("pass %d paid %s, want %s", pass, got, want)
		}
	}
	if f.count(t, "credit_ledger", "entry_type='settle'") != 65 || f.count(t, "usage_charges", "status='settled' AND revision=10") != 1 {
		t.Fatal("multi-source retries duplicated or skipped exact payment history")
	}
}
