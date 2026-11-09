package billing

import (
	"testing"

	"101cloud/backend/internal/platform"
	"gorm.io/gorm"
)

func TestMySQLRegistrationImmediatelyRecordsRealAllowanceGrant(t *testing.T) {
	f := newAdjustmentMySQLFixture(t)
	if err := f.db.Exec("INSERT INTO users(id,username,password_hash,role,status) VALUES(3,'event-registration','unused-test-hash','user','active')").Error; err != nil {
		t.Fatal(err)
	}
	if err := f.db.Exec("UPDATE policy_versions SET config=JSON_SET(config,'$.daily_allowance_pt','3.123456') WHERE id=1").Error; err != nil {
		t.Fatal(err)
	}
	at := now()
	if err := f.db.Transaction(func(db *gorm.DB) error {
		return CreateRegistrationAccount(db, 3, "1", at)
	}); err != nil {
		t.Fatal(err)
	}
	var events []recordedBillingEvent
	if err := f.db.Table("billing_events").Where("user_id=3").Find(&events).Error; err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("registration events=%+v", events)
	}
	e := events[0]
	if e.EventType != "credit.grant" || e.GrantSource.String != "daily" || e.AmountPT.String != "3.123456" || e.AvailableBeforePT.String != "0.000000" || e.AvailableAfterPT.String != "3.123456" || !e.Published {
		t.Fatalf("immediate allowance snapshot=%+v", e)
	}
	if err := dailyGrants(f.ctx, platform.New(f.db, platform.Config{})); err != nil {
		t.Fatal(err)
	}
	var count int64
	if err := f.db.Table("billing_events").Where("user_id=3").Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("registration daily replay count=%d err=%v", count, err)
	}
}
