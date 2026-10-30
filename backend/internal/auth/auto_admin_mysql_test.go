package auth

import (
	"context"
	"sync"
	"testing"
	"time"

	"101cloud/backend/internal/platform"
)

func TestMySQLAutomaticAdministratorCreatedOnlyOnce(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	db := isolatedMySQL(t, ctx)
	var e error
	a := platform.New(db, platform.Config{})
	var wg sync.WaitGroup
	results := make(chan *InitialAdministrator, 4)
	failures := make(chan error, 4)
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			value, err := EnsureAdministrator(ctx, a)
			if err != nil {
				failures <- err
				return
			}
			if value != nil {
				results <- value
			}
		}()
	}
	wg.Wait()
	close(results)
	close(failures)
	for err := range failures {
		t.Fatal(err)
	}
	var initial *InitialAdministrator
	generated := 0
	for value := range results {
		generated++
		initial = value
	}
	if generated != 1 {
		t.Fatalf("concurrent first starts generated %d accounts", generated)
	}
	var account platform.Row
	if e = db.Table("users").Where("role='admin'").Take(&account).Error; e != nil {
		t.Fatal(e)
	}
	if platform.Text(account["username"]) != initial.Username || !verifyPassword(platform.Text(account["password_hash"]), initial.Password) {
		t.Fatal("generated login credentials do not authenticate the committed account")
	}
	var quota uint64
	if e = db.Table("user_limits").Where("user_id=?", account["id"]).Select("max_instances").Scan(&quota).Error; e != nil || quota != 0 {
		t.Fatal("initial administrator has incorrect resource quota")
	}
	passwordHash, e := hashPassword("changed-after-first-login")
	if e != nil {
		t.Fatal(e)
	}
	if e = db.Table("users").Where("id=?", account["id"]).Updates(platform.Row{"password_hash": []byte(passwordHash), "role": "user", "status": "disabled"}).Error; e != nil {
		t.Fatal(e)
	}
	again, e := EnsureAdministrator(ctx, a)
	if e != nil || again != nil {
		t.Fatal("restart recreated credentials after initialization")
	}
	var count int64
	if e = db.Table("users").Count(&count).Error; e != nil || count != 1 {
		t.Fatal("restart duplicated the administrator account")
	}
	if e = db.Table("users").Where("id=?", account["id"]).Take(&account).Error; e != nil {
		t.Fatal(e)
	}
	if !verifyPassword(platform.Text(account["password_hash"]), "changed-after-first-login") {
		t.Fatal("restart reset the existing password")
	}
}
