package instances

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"101cloud/backend/internal/platform"
	cryptsha512 "github.com/sergeymakinen/go-crypt/sha512"
	"golang.org/x/crypto/ssh"
	"gorm.io/gorm"
)

// LoginRequest is accepted only by the final create endpoint, never by quotes,
// task parameters, ordinary instance DTOs or administrator template parameters.
type LoginRequest struct {
	RootPassword  string   `json:"root_password,omitempty"`
	SSHPublicKeys []string `json:"ssh_public_keys,omitempty"`
}

// loginConfig contains no plaintext password. Even the salted password hash is
// sensitive: persist this structure only through App.Encrypt, not as a snapshot.
type loginConfig struct {
	PasswordHash  string   `json:"password_hash,omitempty"`
	SSHPublicKeys []string `json:"ssh_public_keys,omitempty"`
}

func normalizeLogin(in *LoginRequest) (*LoginRequest, error) {
	if in == nil {
		return nil, nil
	}
	if in.RootPassword != "" {
		n := utf8.RuneCountInString(in.RootPassword)
		if !utf8.ValidString(in.RootPassword) || n < 8 || n > 128 || strings.IndexFunc(in.RootPassword, unicode.IsControl) >= 0 {
			return nil, bad("root 密码必须为 8–128 个 Unicode 字符，且不能包含控制字符。")
		}
	}
	if len(in.SSHPublicKeys) > 10 {
		return nil, bad("最多可提供 10 条 SSH 公钥。")
	}
	out := &LoginRequest{RootPassword: in.RootPassword}
	seen := make(map[string]bool, len(in.SSHPublicKeys))
	for _, input := range in.SSHPublicKeys {
		if len(input) > 8192 || strings.ContainsAny(input, "\r\n") || strings.IndexFunc(input, unicode.IsControl) >= 0 || !utf8.ValidString(input) {
			return nil, bad("SSH 公钥必须为不超过 8192 字节的单行 OpenSSH 公钥。")
		}
		text := strings.TrimSpace(input)
		fields := strings.Fields(text)
		if len(fields) < 2 || (fields[0] != "ssh-ed25519" && fields[0] != "ssh-rsa" && fields[0] != "ecdsa-sha2-nistp256" && fields[0] != "ecdsa-sha2-nistp384" && fields[0] != "ecdsa-sha2-nistp521") {
			return nil, bad("仅支持 Ed25519、RSA（至少 2048 位）和 ECDSA nistp256/384/521 公钥；不能包含私钥或 authorized_keys 选项。")
		}
		key, _, options, rest, err := ssh.ParseAuthorizedKey([]byte(text))
		if err != nil || len(options) != 0 || len(strings.TrimSpace(string(rest))) != 0 || key.Type() != fields[0] {
			return nil, bad("SSH 公钥格式无效；不能包含私钥、登录选项或多行内容。")
		}
		cryptoKey, ok := key.(ssh.CryptoPublicKey)
		if !ok {
			return nil, bad("不支持该 SSH 公钥类型。")
		}
		switch public := cryptoKey.CryptoPublicKey().(type) {
		case ed25519.PublicKey:
		case *rsa.PublicKey:
			if public.N.BitLen() < 2048 {
				return nil, bad("RSA 公钥必须至少为 2048 位。")
			}
		case *ecdsa.PublicKey:
			if public.Curve.Params().Name != "P-256" && public.Curve.Params().Name != "P-384" && public.Curve.Params().Name != "P-521" {
				return nil, bad("不支持该 ECDSA 曲线。")
			}
		default:
			return nil, bad("不支持该 SSH 公钥类型。")
		}
		// Drop comments, including YAML-looking text: only a parsed canonical key
		// can reach cloud-init. Duplicate keys with different comments collapse.
		canonical := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key)))
		if !seen[canonical] {
			out.SSHPublicKeys = append(out.SSHPublicKeys, canonical)
			seen[canonical] = true
		}
	}
	if out.RootPassword == "" && len(out.SSHPublicKeys) == 0 {
		return nil, bad("login 至少需要一种非空 root 密码或 SSH 公钥。")
	}
	return out, nil
}

func sealLogin(a *platform.App, in *LoginRequest) ([]byte, error) {
	cfg := loginConfig{SSHPublicKeys: in.SSHPublicKeys}
	if in.RootPassword != "" {
		hash, err := cryptsha512.NewHash(in.RootPassword, cryptsha512.DefaultRounds)
		if err != nil {
			return nil, errors.New("cannot hash instance login password")
		}
		cfg.PasswordHash = hash
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		return nil, errors.New("cannot encode instance login configuration")
	}
	sealed, err := a.Encrypt(raw)
	clear(raw)
	if err != nil {
		return nil, errors.New("cannot encrypt instance login configuration")
	}
	return sealed, nil
}

func loginUserData(a *platform.App, r platform.Row) (string, error) {
	if platform.Text(r["login_config_ciphertext"]) == "" {
		return "", nil
	}
	raw, err := a.Decrypt([]byte(platform.Text(r["login_config_ciphertext"])))
	if err != nil {
		return "", errors.New("cannot decrypt instance login configuration")
	}
	defer clear(raw)
	var cfg loginConfig
	if json.Unmarshal(raw, &cfg) != nil || (cfg.PasswordHash == "" && len(cfg.SSHPublicKeys) == 0) {
		return "", errors.New("invalid encrypted instance login configuration")
	}
	password := cfg.PasswordHash != ""
	// '*' is an unusable password without the leading '!' account lock that
	// rejects public-key login in OpenSSH with UsePAM=no on some images.
	hash, mode := cfg.PasswordHash, "password\n"
	if !password {
		hash, mode = "*", "ssh-key\n"
	}
	root := platform.Row{"name": "root", "hashed_passwd": hash, "lock_passwd": false}
	if len(cfg.SSHPublicKeys) > 0 {
		root["ssh_authorized_keys"] = cfg.SSHPublicKeys
	}
	cloud := platform.Row{
		"users": []platform.Row{root}, "disable_root": false, "ssh_pwauth": password,
		"chpasswd":    platform.Row{"expire": false},
		"write_files": []platform.Row{{"path": "/etc/101cloud/ssh-login-mode", "owner": "root:root", "permissions": "0600", "content": mode}},
	}
	// JSON is YAML-compatible. Never interpolate user strings into YAML or a shell.
	return "#cloud-config\n" + jsonText(cloud) + "\n", nil
}

// Login completion has its own durable checkpoint, independent of an optional
// administrator template. Reboots neither reset passwords nor rerun cloud-init.
func workerLoginInitialize(ctx context.Context, a *platform.App, j platform.Job, r platform.Row) error {
	if platform.Text(r["login_config_ciphertext"]) == "" || !platform.Stamp(r["login_initialized_at"]).IsZero() {
		return nil
	}
	timeout, cancel := context.WithTimeout(ctx, 610*time.Second)
	defer cancel()
	result, err := workerOperation(timeout, a, j, "login.cloud-ready", func() (map[string]any, error) {
		current, e := workerInstance(timeout, a, j)
		if e != nil {
			return nil, e
		}
		if platform.Text(current["desired_state"]) != "running" {
			return nil, workerSkipped
		}
		if _, _, e = workerObserve(timeout, a, r); e != nil {
			return nil, e
		}
		return a.Agent(timeout, j.NodeID, http.MethodPost, workerPath(r, "/exec"), platform.Row{
			// A disabled cloud-init can exit zero. Require the first-boot
			// completion file and our non-sensitive marker as well. This fixed
			// command contains no caller input, password, key or user-data.
			"command":     []string{"/usr/bin/timeout", "--signal=KILL", "600", "/bin/sh", "-c", "/usr/bin/cloud-init status --wait >/dev/null 2>&1 && test -f /var/lib/cloud/instance/boot-finished && test -f /etc/101cloud/ssh-login-mode"},
			"environment": platform.Row{"PATH": "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"},
			"interactive": false, "wait-for-websocket": false, "record-output": false,
		})
	}, nil)
	if err != nil {
		return platform.Unknown(err)
	}
	if err = workerExit(result); err != nil {
		return platform.Unknown(err)
	}
	return a.DB.WithContext(ctx).Transaction(func(db *gorm.DB) error {
		if e := lockWorkerAccount(db, a, j, j.UserID); e != nil {
			return e
		}
		return db.Table("instances").Where("id=? AND login_initialized_at IS NULL", j.InstanceID).Updates(platform.Row{"login_initialized_at": a.Time(), "revision": gorm.Expr("revision+1")}).Error
	})
}
