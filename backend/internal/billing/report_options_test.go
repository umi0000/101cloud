package billing

import (
	"net/url"
	"strings"
	"testing"
)

func TestMySQLBillingInstanceOptionsOwnedHistoryWithoutWallet(t *testing.T) {
	f := reportFixture(t)
	for _, id := range []uint64{2, 3, 4} {
		i := debtRow(t, f.db, "instances", 1)
		i["id"], i["incus_name"], i["logical_name"] = id, "option-instance-"+sid(id), "Literal %_ "+sid(id)
		if id == 2 {
			i["deleted_at"], i["desired_state"], i["observed_state"] = now(), "deleted", "deleted"
		}
		if id == 4 {
			i["user_id"] = 1
		}
		if err := f.db.Table("instances").Create(&i).Error; err != nil {
			t.Fatal(err)
		}
	}
	// An identity-only request remains available without an explicit wallet.
	if err := f.db.Exec("DELETE FROM usage_sessions WHERE account_id=1").Error; err != nil {
		t.Fatal(err)
	}
	if err := f.db.Exec("DELETE FROM credit_accounts WHERE user_id=2").Error; err != nil {
		t.Fatal(err)
	}
	before := map[string]int64{}
	for _, table := range []string{"usage_charges", "credit_ledger", "credit_buckets", "credit_reservations", "tasks"} {
		before[table] = f.count(t, table, "1=1")
	}
	var out billingInstanceOptionsDTO
	reportCall(t, f, "/billing/instance-options?page_size=1", 200, &out)
	if out.Total != 3 || len(out.Items) != 1 || out.Items[0].InstanceID != "3" || out.AsOf.IsZero() || out.Page != 1 || out.PageSize != 1 {
		t.Fatalf("first page: %+v", out)
	}
	reportCall(t, f, "/billing/instance-options?page_size=1&page=2", 200, &out)
	if len(out.Items) != 1 || out.Items[0].InstanceID != "2" || !out.Items[0].Deleted {
		t.Fatalf("historical page: %+v", out)
	}
	reportCall(t, f, "/billing/instance-options?search="+url.QueryEscape("%_"), 200, &out)
	if out.Total != 2 || len(out.Items) != 2 {
		t.Fatalf("literal search: %+v", out)
	}
	reportCall(t, f, "/billing/instance-options?search=2", 200, &out)
	if out.Total != 1 || out.Items[0].InstanceID != "2" {
		t.Fatalf("ID search: %+v", out)
	}
	reportCall(t, f, "/billing/instance-options?search=4", 200, &out)
	if out.Total != 0 || len(out.Items) != 0 {
		t.Fatalf("foreign identity leaked: %+v", out)
	}
	reportCall(t, f, "/billing/instance-options?page=99", 200, &out)
	if out.Total != 3 || len(out.Items) != 0 {
		t.Fatalf("empty page: %+v", out)
	}
	for _, query := range []string{"period=year", "user_id=1", "instance_id=2", "page=0", "page=-1", "page=", "page=2147483648", "page_size=101", "page_size=0", "search=a&search=b", "page=1&page=2", "search=" + url.QueryEscape(strings.Repeat("界", 129)), "search=%zz"} {
		reportCall(t, f, "/billing/instance-options?"+query, 422, nil)
	}
	for table, count := range before {
		if f.count(t, table, "1=1") != count {
			t.Fatalf("identity GET wrote %s", table)
		}
	}
}
