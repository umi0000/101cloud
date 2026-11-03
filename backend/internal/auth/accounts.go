package auth

import (
	"net/http"
	"strconv"

	"101cloud/backend/internal/billing"
	"101cloud/backend/internal/platform"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func registerMe(a *platform.App, user *gin.RouterGroup) {
	user.GET("/me", platform.Handle(func(c *gin.Context) (any, error) {
		db := platform.DB(c, a)
		row, err := platform.Get(db, "users", platform.Current(c).ID, false)
		if err != nil {
			return nil, err
		}
		limits, err := getLimits(db, platform.Current(c).ID, false)
		if err != nil {
			return nil, err
		}
		platform.SetETag(c, row)
		return gin.H{"user": userDTO(row), "limits": limitsDTO(limits), "effective_permissions": permissions(row)}, nil
	}))
	user.PATCH("/me", platform.Handle(func(c *gin.Context) (any, error) {
		var input map[string]any
		if err := platform.Decode(c, &input); err != nil {
			return nil, err
		}
		if err := platform.ValidateKeys(input, "email"); err != nil {
			return nil, err
		}
		if len(input) == 0 {
			return nil, validation("At least one writable field is required")
		}
		email, err := emailValue(input["email"])
		if err != nil {
			return nil, err
		}
		row, err := platform.Get(platform.DB(c, a), "users", platform.Current(c).ID, true)
		if err != nil {
			return nil, err
		}
		updated, err := platform.Save(c, a, "users", row, platform.Row{"email": email})
		if err != nil {
			return nil, err
		}
		platform.SetETag(c, updated)
		return userDTO(updated), nil
	}))
	user.POST("/me/password", platform.Handle(func(c *gin.Context) (any, error) {
		var input struct {
			CurrentPassword string `json:"current_password"`
			NewPassword     string `json:"new_password"`
		}
		if err := platform.Decode(c, &input); err != nil {
			return nil, err
		}
		if !validPassword(input.NewPassword) {
			return nil, validation("Password must contain 12–128 characters")
		}
		if err := loginLimit(c, a, "account:"+strconv.FormatUint(platform.Current(c).ID, 10)); err != nil {
			return nil, err
		}
		row, err := platform.Get(platform.DB(c, a), "users", platform.Current(c).ID, true)
		if err != nil {
			return nil, err
		}
		if err := platform.Revision(c, row); err != nil {
			return nil, err
		}
		if !verifyPassword(platform.Text(row["password_hash"]), input.CurrentPassword) {
			return nil, invalidCredentials()
		}
		hash, err := hashPassword(input.NewPassword)
		if err != nil {
			return nil, err
		}
		if _, err := platform.Save(c, a, "users", row, platform.Row{"password_hash": []byte(hash)}); err != nil {
			return nil, err
		}
		if err := revokeAll(platform.DB(c, a), a, platform.Current(c).ID); err != nil {
			return nil, err
		}
		clearSessionCookies(c, a)
		noContent(c)
		return nil, nil
	}))
	user.GET("/me/sessions", platform.Handle(func(c *gin.Context) (any, error) {
		current := platform.Current(c)
		query := platform.DB(c, a).Table("user_sessions").
			Select("id,revision,created_at,expires_at").
			Where("user_id = ? AND revoked_at IS NULL AND expires_at > ?", current.ID, a.Time())
		return platform.ListMap(c, a, query, nil, func(r platform.Row) (platform.Row, error) {
			out := platform.Public(r)
			out["is_current"] = platform.Uint(r["id"]) == current.SessionID
			return out, nil
		})
	}))
	user.DELETE("/me/sessions/:session_id", platform.Handle(func(c *gin.Context) (any, error) {
		id, err := platform.ID(c, "session_id")
		if err != nil {
			return nil, err
		}
		row, err := platform.Get(platform.DB(c, a), "user_sessions", id, true)
		if err != nil {
			return nil, err
		}
		if platform.Uint(row["user_id"]) != platform.Current(c).ID {
			return nil, platform.E(404, "not_found", "Session not found")
		}
		if _, err := platform.Save(c, a, "user_sessions", row, platform.Row{"revoked_at": a.Time()}); err != nil {
			return nil, err
		}
		if id == platform.Current(c).SessionID {
			clearSessionCookies(c, a)
		}
		noContent(c)
		return nil, nil
	}))
}

func registerAdmin(a *platform.App, admin *gin.RouterGroup) {
	admin.GET("/users", platform.Handle(func(c *gin.Context) (any, error) {
		query := platform.DB(c, a).Table("users").Select("id,username,email,role,status,revision,created_at")
		for _, key := range []string{"status", "role", "username"} {
			if value, ok := c.GetQuery(key); ok {
				if (key == "status" && value != "active" && value != "disabled" && value != "deleted") || (key == "role" && value != "user" && value != "admin") || (key == "username" && !validUsername(value)) {
					return nil, validation("Invalid " + key + " filter")
				}
				query = query.Where(key+" = ?", value)
			}
		}
		return platform.List(c, a, query, []string{"status", "role", "username"})
	}))
	admin.POST("/users", platform.Handle(func(c *gin.Context) (any, error) {
		var input struct {
			Username              string  `json:"username"`
			Email                 *string `json:"email"`
			InitialPassword       string  `json:"initial_password"`
			Role                  string  `json:"role"`
			CreditPolicyVersionID string  `json:"credit_policy_version_id"`
		}
		if err := platform.Decode(c, &input); err != nil {
			return nil, err
		}
		if !validUsername(input.Username) {
			return nil, validation("Username must contain 1–64 characters")
		}
		if input.Role != "user" && input.Role != "admin" {
			return nil, validation("Role must be user or admin")
		}
		if input.Email != nil && !validEmail(*input.Email) {
			return nil, validation("Invalid email address")
		}
		if _, err := platform.IDString(input.CreditPolicyVersionID); err != nil {
			return nil, err
		}
		hash, err := hashPassword(input.InitialPassword)
		if err != nil {
			return nil, err
		}
		db := platform.DB(c, a)
		if err := lockAdminGuard(db); err != nil {
			return nil, err
		}
		row := map[string]any{"username": input.Username, "email": input.Email, "password_hash": []byte(hash), "role": input.Role, "status": "active", "created_at": a.Time(), "updated_at": a.Time()}
		if err := db.Table("users").Create(row).Error; err != nil {
			return nil, platform.WrapDB(err)
		}
		var created struct{ ID uint64 }
		if err := db.Table("users").Select("id").Where("username = ?", input.Username).Take(&created).Error; err != nil {
			return nil, platform.WrapDB(err)
		}
		if err := db.Table("user_limits").Create(zeroLimits(created.ID)).Error; err != nil {
			return nil, platform.WrapDB(err)
		}
		if _, err := billing.CreateAccount(db, created.ID, input.CreditPolicyVersionID); err != nil {
			return nil, err
		}
		result, err := platform.Get(db, "users", created.ID, false)
		if err != nil {
			return nil, err
		}
		platform.SetETag(c, result)
		c.Header("Location", "/api/v1/admin/users/"+strconv.FormatUint(created.ID, 10))
		platform.Reply(c, http.StatusCreated, userDTO(result))
		return nil, nil
	}))
	admin.GET("/users/:id", platform.Handle(func(c *gin.Context) (any, error) {
		id, err := platform.ID(c, "id")
		if err != nil {
			return nil, err
		}
		row, err := platform.Get(platform.DB(c, a), "users", id, false)
		if err != nil {
			return nil, err
		}
		platform.SetETag(c, row)
		return userDTO(row), nil
	}))
	admin.PATCH("/users/:id", platform.Handle(func(c *gin.Context) (any, error) {
		id, err := platform.ID(c, "id")
		if err != nil {
			return nil, err
		}
		var input map[string]any
		if err := platform.Decode(c, &input); err != nil {
			return nil, err
		}
		if err := platform.ValidateKeys(input, "email", "status", "role"); err != nil {
			return nil, err
		}
		if len(input) == 0 {
			return nil, validation("At least one writable field is required")
		}
		changes := platform.Row{}
		if value, ok := input["email"]; ok {
			email, err := emailValue(value)
			if err != nil {
				return nil, err
			}
			changes["email"] = email
		}
		if value, ok := input["status"]; ok {
			if value != "active" && value != "disabled" {
				return nil, validation("Status must be active or disabled")
			}
			changes["status"] = value
		}
		if value, ok := input["role"]; ok {
			if value != "user" && value != "admin" {
				return nil, validation("Role must be user or admin")
			}
			changes["role"] = value
		}
		db := platform.DB(c, a)
		// Every admin membership mutation takes this singleton before user locks.
		if err := lockAdminGuard(db); err != nil {
			return nil, err
		}
		row, err := platform.Get(db, "users", id, true)
		if err != nil {
			return nil, err
		}
		if err := platform.Revision(c, row); err != nil {
			return nil, err
		}
		if platform.Text(row["status"]) == "deleted" {
			return nil, platform.E(409, "invalid_state", "Deleted accounts cannot be modified")
		}
		demoting := changes["role"] == "user" || changes["status"] == "disabled"
		if platform.Text(row["role"]) == "admin" && platform.Text(row["status"]) == "active" && demoting {
			var count int64
			if err := db.Table("users").Where("role = 'admin' AND status = 'active'").Count(&count).Error; err != nil {
				return nil, platform.WrapDB(err)
			}
			if count <= 1 {
				return nil, platform.E(409, "invalid_state", "The last active administrator cannot be disabled or demoted")
			}
		}
		result, err := platform.Save(c, a, "users", row, changes)
		if err != nil {
			return nil, err
		}
		// Role changes also invalidate existing privileged websocket connections.
		if changes["status"] == "disabled" || (changes["role"] != nil && changes["role"] != platform.Text(row["role"])) {
			if err := revokeAll(db, a, id); err != nil {
				return nil, err
			}
		}
		platform.SetETag(c, result)
		return userDTO(result), nil
	}))
	admin.POST("/users/:id/reset-password", platform.Handle(func(c *gin.Context) (any, error) {
		id, err := platform.ID(c, "id")
		if err != nil {
			return nil, err
		}
		var input struct {
			NewPassword string `json:"new_password"`
		}
		if err := platform.Decode(c, &input); err != nil {
			return nil, err
		}
		hash, err := hashPassword(input.NewPassword)
		if err != nil {
			return nil, err
		}
		db := platform.DB(c, a)
		row, err := platform.Get(db, "users", id, true)
		if err != nil {
			return nil, err
		}
		if platform.Text(row["status"]) == "deleted" {
			return nil, platform.E(409, "invalid_state", "Deleted accounts cannot be modified")
		}
		if _, err := platform.Save(c, a, "users", row, platform.Row{"password_hash": []byte(hash)}); err != nil {
			return nil, err
		}
		if err := revokeAll(db, a, id); err != nil {
			return nil, err
		}
		if err := platform.Audit(c.Request.Context(), db, platform.Current(c).ID, 0, 0, "auth.reset-password", gin.H{"user_id": strconv.FormatUint(id, 10)}); err != nil {
			return nil, err
		}
		if id == platform.Current(c).ID {
			clearSessionCookies(c, a)
		}
		noContent(c)
		return nil, nil
	}))
	admin.GET("/users/:id/limits", platform.Handle(func(c *gin.Context) (any, error) {
		id, err := platform.ID(c, "id")
		if err != nil {
			return nil, err
		}
		if _, err := platform.Get(platform.DB(c, a), "users", id, false); err != nil {
			return nil, err
		}
		row, err := getLimits(platform.DB(c, a), id, false)
		if err != nil {
			return nil, err
		}
		platform.SetETag(c, row)
		return limitsDTO(row), nil
	}))
	admin.PUT("/users/:id/limits", platform.Handle(func(c *gin.Context) (any, error) {
		id, err := platform.ID(c, "id")
		if err != nil {
			return nil, err
		}
		var input struct {
			MaxInstances         *uint32 `json:"max_instances"`
			MaxVCPUs             *uint32 `json:"max_vcpus"`
			MaxMemoryBytes       *string `json:"max_memory_bytes"`
			MaxDiskBytes         *string `json:"max_disk_bytes"`
			MaxGPUs              *uint32 `json:"max_gpus"`
			MaxResidentInstances *uint32 `json:"max_resident_instances"`
		}
		if err := platform.Decode(c, &input); err != nil {
			return nil, err
		}
		if input.MaxInstances == nil || input.MaxVCPUs == nil || input.MaxMemoryBytes == nil || input.MaxDiskBytes == nil || input.MaxGPUs == nil || input.MaxResidentInstances == nil {
			return nil, validation("The complete limits object is required")
		}
		if *input.MaxResidentInstances > *input.MaxInstances {
			return nil, validation("Custom instance quota cannot exceed total instance quota")
		}
		memory, err := capacity(*input.MaxMemoryBytes)
		if err != nil {
			return nil, err
		}
		disk, err := capacity(*input.MaxDiskBytes)
		if err != nil {
			return nil, err
		}
		db := platform.DB(c, a)
		if _, err := platform.Get(db, "users", id, true); err != nil {
			return nil, err
		}
		row, err := getLimits(db, id, true)
		if err != nil {
			return nil, err
		}
		changes := platform.Row{"max_instances": *input.MaxInstances, "max_vcpus": *input.MaxVCPUs, "max_memory_bytes": memory, "max_disk_bytes": disk, "max_gpu_allocations": *input.MaxGPUs, "max_resident_instances": *input.MaxResidentInstances}
		result, err := platform.Save(c, a, "user_limits", row, changes)
		if err != nil {
			return nil, err
		}
		platform.SetETag(c, result)
		return limitsDTO(result), nil
	}))
}

func getLimits(db *gorm.DB, userID uint64, lock bool) (platform.Row, error) {
	query := db.Table("user_limits").Where("user_id = ?", userID)
	if lock {
		query = query.Clauses(clause.Locking{Strength: "UPDATE"})
	}
	row := platform.Row{}
	if err := query.Take(&row).Error; err != nil {
		return nil, platform.WrapDB(err)
	}
	return row, nil
}
func limitsDTO(row platform.Row) gin.H {
	return gin.H{"id": strconv.FormatUint(platform.Uint(row["id"]), 10), "user_id": strconv.FormatUint(platform.Uint(row["user_id"]), 10), "revision": strconv.FormatUint(platform.Uint(row["revision"]), 10), "max_instances": platform.Uint(row["max_instances"]), "max_resident_instances": platform.Uint(row["max_resident_instances"]), "max_vcpus": platform.Uint(row["max_vcpus"]), "max_memory_bytes": strconv.FormatUint(platform.Uint(row["max_memory_bytes"]), 10), "max_disk_bytes": strconv.FormatUint(platform.Uint(row["max_disk_bytes"]), 10), "max_gpus": platform.Uint(row["max_gpu_allocations"])}
}
func permissions(row platform.Row) []string {
	effective := []string{"catalog:read", "instances:own", "billing:own", "sessions:own", "profile:own"}
	if platform.Text(row["role"]) == "admin" {
		effective = append(effective, "admin")
	}
	return effective
}
func emailValue(value any) (any, error) {
	if value == nil {
		return nil, nil
	}
	email, ok := value.(string)
	if !ok || !validEmail(email) {
		return nil, validation("Invalid email address")
	}
	return email, nil
}
func capacity(value string) (uint64, error) {
	n, err := strconv.ParseUint(value, 10, 64)
	if err != nil || strconv.FormatUint(n, 10) != value {
		return 0, validation("Capacities must be nonnegative decimal integer strings")
	}
	return n, nil
}
func revokeAll(db *gorm.DB, a *platform.App, userID uint64) error {
	return platform.WrapDB(db.Table("user_sessions").Where("user_id = ? AND revoked_at IS NULL", userID).
		Updates(map[string]any{"revoked_at": a.Time(), "revision": gorm.Expr("revision + 1")}).Error)
}
