package auth

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"101cloud/backend/internal/billing"
	"101cloud/backend/internal/platform"
	"github.com/gin-gonic/gin"
	mysqlerr "github.com/go-sql-driver/mysql"
	"gorm.io/gorm"
)

var registrationLimitColumns = []string{"max_instances", "max_vcpus", "max_memory_bytes", "max_disk_bytes", "max_gpu_allocations", "max_resident_instances"}

func registerRegistration(a *platform.App, api, admin *gin.RouterGroup) {
	api.GET("/auth/registration", platform.Handle(func(c *gin.Context) (any, error) {
		c.Header("Cache-Control", "no-store")
		db := a.DB.WithContext(c.Request.Context())
		settings, err := platform.Get(db, "registration_settings", 1, false)
		if err != nil {
			return nil, err
		}
		enabled, err := registrationAvailable(db, settings, false)
		return gin.H{"enabled": enabled}, err
	}))
	api.POST("/auth/register", platform.Handle(func(c *gin.Context) (any, error) {
		return selfRegister(c, a)
	}))
	admin.GET("/registration-settings", platform.Handle(func(c *gin.Context) (any, error) {
		settings, err := platform.Get(platform.DB(c, a), "registration_settings", 1, false)
		if err != nil {
			return nil, err
		}
		platform.SetETag(c, settings)
		return registrationSettingsDTO(settings), nil
	}))
	admin.PUT("/registration-settings", platform.Handle(func(c *gin.Context) (any, error) {
		var input struct {
			Enabled               *bool           `json:"enabled"`
			CreditPolicyVersionID json.RawMessage `json:"credit_policy_version_id"`
			Limits                *limitsInput    `json:"limits"`
		}
		if err := platform.Decode(c, &input); err != nil {
			return nil, err
		}
		if input.Enabled == nil || len(input.CreditPolicyVersionID) == 0 || input.Limits == nil {
			return nil, validation("The complete registration settings object is required")
		}
		var policyID *string
		if err := json.Unmarshal(input.CreditPolicyVersionID, &policyID); err != nil {
			return nil, validation("Credit policy version ID must be a decimal string or null")
		}
		changes, err := input.Limits.values()
		if err != nil {
			return nil, err
		}
		db := platform.DB(c, a)
		settings, err := platform.Get(db, "registration_settings", 1, true)
		if err != nil {
			return nil, err
		}
		if err := platform.Revision(c, settings); err != nil {
			return nil, err
		}
		changes["enabled"] = *input.Enabled
		changes["credit_policy_version_id"] = nil
		if policyID != nil {
			// Disabled settings may retain an inactive policy, but never a wrong-kind ID.
			if _, _, err := platform.PolicySource(db, "credit", *policyID, true); err != nil {
				return nil, err
			}
			changes["credit_policy_version_id"] = *policyID
		}
		if *input.Enabled {
			if policyID == nil {
				return nil, validation("An active, effective credit policy is required to enable registration")
			}
			if _, err := billing.ValidateAccountPolicy(db, *policyID, true); err != nil {
				return nil, err
			}
		}
		updated, err := platform.Save(c, a, "registration_settings", settings, changes)
		if err != nil {
			return nil, err
		}
		return registrationSettingsDTO(updated), nil
	}))
}

func registrationSettingsDTO(settings platform.Row) gin.H {
	limits := limitsDTO(settings)
	delete(limits, "id")
	delete(limits, "user_id")
	delete(limits, "revision")
	var policyID any
	if settings["credit_policy_version_id"] != nil {
		policyID = strconv.FormatUint(platform.Uint(settings["credit_policy_version_id"]), 10)
	}
	return gin.H{"id": strconv.FormatUint(platform.Uint(settings["id"]), 10), "revision": strconv.FormatUint(platform.Uint(settings["revision"]), 10), "enabled": platform.Uint(settings["enabled"]) != 0, "credit_policy_version_id": policyID, "limits": limits}
}

func registrationAvailable(db *gorm.DB, settings platform.Row, lock bool) (bool, error) {
	if platform.Uint(settings["enabled"]) == 0 || settings["credit_policy_version_id"] == nil {
		return false, nil
	}
	_, err := billing.ValidateAccountPolicy(db, strconv.FormatUint(platform.Uint(settings["credit_policy_version_id"]), 10), lock)
	var rejection *platform.HTTPError
	if errors.As(err, &rejection) && rejection.Status == http.StatusUnprocessableEntity {
		return false, nil
	}
	return err == nil, err
}

func registrationDisabled() error {
	return platform.E(http.StatusForbidden, "registration_disabled", "Registration is not available")
}

func selfRegister(c *gin.Context, a *platform.App) (any, error) {
	c.Header("Cache-Control", "no-store")
	var input struct {
		Username string  `json:"username"`
		Password string  `json:"password"`
		Email    *string `json:"email"`
	}
	if err := platform.Decode(c, &input); err != nil {
		return nil, err
	}
	// Counters commit independently, before password hashing or any domain transaction.
	if err := authAttemptLimit(c, a, "registration", input.Username, "Too many registration attempts"); err != nil {
		return nil, err
	}
	if !validUsername(input.Username) {
		return nil, validation("Username must contain 1–64 characters")
	}
	if !validPassword(input.Password) {
		return nil, validation("Password must contain 12–128 characters")
	}
	if input.Email != nil && !validEmail(*input.Email) {
		return nil, validation("Invalid email address")
	}
	db := a.DB.WithContext(c.Request.Context())
	settings, err := platform.Get(db, "registration_settings", 1, false)
	if err != nil {
		return nil, err
	}
	available, err := registrationAvailable(db, settings, false)
	if err != nil {
		return nil, err
	}
	if !available {
		return nil, registrationDisabled()
	}
	hash, err := hashPassword(input.Password)
	if err != nil {
		return nil, err
	}
	var user platform.Row
	err = db.Transaction(func(tx *gorm.DB) error {
		// Match administrator creation/bootstrap lock ordering before inserting users.
		if err := lockAdminGuard(tx); err != nil {
			return err
		}
		settings, err := platform.Get(tx, "registration_settings", 1, true)
		if err != nil {
			return err
		}
		available, err := registrationAvailable(tx, settings, true)
		if err != nil {
			return err
		}
		if !available {
			return registrationDisabled()
		}
		row := map[string]any{"username": input.Username, "email": input.Email, "password_hash": []byte(hash), "role": "user", "status": "active", "created_at": a.Time(), "updated_at": a.Time()}
		if err := tx.Table("users").Create(row).Error; err != nil {
			var duplicate *mysqlerr.MySQLError
			if errors.As(err, &duplicate) && duplicate.Number == 1062 && strings.Contains(duplicate.Message, "uq_users_username") {
				return platform.E(http.StatusConflict, "username_taken", "Username is already taken")
			}
			return platform.WrapDB(err)
		}
		id, err := platform.InsertedID(row)
		if err != nil {
			return err
		}
		limits := zeroLimits(id)
		for _, key := range registrationLimitColumns {
			limits[key] = settings[key]
		}
		if err := tx.Table("user_limits").Create(limits).Error; err != nil {
			return platform.WrapDB(err)
		}
		if err := billing.CreateRegistrationAccount(tx, id, strconv.FormatUint(platform.Uint(settings["credit_policy_version_id"]), 10), a.Time()); err != nil {
			return err
		}
		user, err = platform.Get(tx, "users", id, false)
		if err != nil {
			return err
		}
		return platform.Audit(c.Request.Context(), tx, id, 0, 0, "auth.register", gin.H{"settings_revision": strconv.FormatUint(platform.Uint(settings["revision"]), 10)})
	})
	if err != nil {
		return nil, platform.WrapDB(err)
	}
	platform.Reply(c, http.StatusCreated, userDTO(user))
	return nil, nil
}
