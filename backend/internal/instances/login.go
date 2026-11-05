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
	PasswordHash  string   `json:"password_hash"`
	SSHPublicKeys []string `json:"ssh_public_keys,omitempty"`
}

func normalizeLogin(in *LoginRequest) (*LoginRequest, error) {
	if in == nil {
		return nil, nil
	}
	if (in.RootPassword != "") == (len(in.SSHPublicKeys) != 0) {
		return nil, bad("root 密码和 SSH 公钥必须二选一。")
	}
	if in.RootPassword != "" {
		n := utf8.RuneCountInString(in.RootPassword)
		if !utf8.ValidString(in.RootPassword) || n < 8 || n > 128 || strings.IndexFunc(in.RootPassword, unicode.IsControl) >= 0 {
			return nil, bad("root 密码必须为 8–128 个 Unicode 字符，且不能包含控制字符。")
		}
	}
	out := &LoginRequest{RootPassword: in.RootPassword}
	keys, err := normalizeSSHPublicKeys(in.SSHPublicKeys)
	if err != nil {
		return nil, err
	}
	out.SSHPublicKeys = keys
	return out, nil
}

func normalizeSSHPublicKeys(entries []string) ([]string, error) {
	invalid := func() error {
		return bad("SSH 公钥最多 10 条，每条最多 8192 字节；仅支持 Ed25519、RSA（至少 2048 位）及 ECDSA nistp256/384/521 公钥，不接受私钥、证书或选项。")
	}
	if len(entries) > 10 {
		return nil, invalid()
	}
	var keys []string
	seen := make(map[string]bool, len(entries))
	for _, entry := range entries {
		if len(entry) > 8192 || !utf8.ValidString(entry) || strings.ContainsAny(entry, "\r\n\x00") {
			return nil, invalid()
		}
		key, _, options, rest, err := ssh.ParseAuthorizedKey([]byte(entry))
		if err != nil || len(options) != 0 || len(rest) != 0 {
			return nil, invalid()
		}
		trimmed := strings.TrimSpace(entry)
		separator := strings.IndexAny(trimmed, " \t")
		if separator < 0 || trimmed[:separator] != key.Type() {
			return nil, invalid()
		}
		cryptoKey, ok := key.(ssh.CryptoPublicKey)
		if !ok {
			return nil, invalid()
		}
		switch public := cryptoKey.CryptoPublicKey().(type) {
		case ed25519.PublicKey:
			if key.Type() != ssh.KeyAlgoED25519 {
				return nil, invalid()
			}
		case *rsa.PublicKey:
			if key.Type() != ssh.KeyAlgoRSA || public.N.BitLen() < 2048 || public.N.Bit(0) != 1 || public.E < 3 || public.E%2 == 0 {
				return nil, invalid()
			}
		case *ecdsa.PublicKey:
			if key.Type() != ssh.KeyAlgoECDSA256 && key.Type() != ssh.KeyAlgoECDSA384 && key.Type() != ssh.KeyAlgoECDSA521 {
				return nil, invalid()
			}
		default:
			return nil, invalid()
		}
		canonical := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key)))
		if !seen[canonical] {
			seen[canonical] = true
			keys = append(keys, canonical)
		}
	}
	return keys, nil
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
	// Even network-only creates must override the image's default SSH modules.
	// Preserve embedded host keys; neither generate, publish nor fingerprint keys.
	// An explicit empty ssh_keys mapping selects cc_ssh's non-generation branch.
	// ssh_genkeytypes: [] is rejected by supported cloud-init schemas as minItems=1.
	cloud := platform.Row{
		"preserve_hostname": true,
		"ssh_deletekeys":    false, "ssh_keys": platform.Row{},
		"ssh":                 platform.Row{"emit_keys_to_console": false},
		"no_ssh_fingerprints": true, "allow_public_ssh_keys": false,
		"ssh_publish_hostkeys": platform.Row{"enabled": false},
	}
	if platform.Text(r["login_config_ciphertext"]) != "" {
		raw, err := a.Decrypt([]byte(platform.Text(r["login_config_ciphertext"])))
		if err != nil {
			return "", errors.New("cannot decrypt instance login configuration")
		}
		defer clear(raw)
		var cfg loginConfig
		if json.Unmarshal(raw, &cfg) != nil {
			return "", errors.New("invalid encrypted instance login configuration")
		}
		if cfg.PasswordHash == "" && len(cfg.SSHPublicKeys) == 0 {
			return "", errors.New("empty encrypted instance login configuration")
		}
		hash := cfg.PasswordHash
		if hash == "" {
			hash = "*"
		} else if !strings.HasPrefix(hash, "$6$") {
			return "", errors.New("invalid encrypted instance password hash")
		}
		cloud["users"] = []platform.Row{{"name": "root", "hashed_passwd": hash, "lock_passwd": false}}
		if len(cfg.SSHPublicKeys) != 0 {
			keys, err := normalizeSSHPublicKeys(cfg.SSHPublicKeys)
			if err != nil {
				return "", errors.New("invalid encrypted instance public keys")
			}
			cloud["users"].([]platform.Row)[0]["ssh_authorized_keys"] = keys
			// cc_ssh must not add forced-command restrictions to supplied root keys.
			cloud["disable_root"] = false
		}
		cloud["chpasswd"] = platform.Row{"expire": false}
	}
	// JSON is YAML-compatible. Never interpolate user strings into YAML or a shell.
	return "#cloud-config\n" + jsonText(cloud) + "\n", nil
}

// CloudInitPasswordReadyScript reads only guest-local state and returns a bool.
// Hashes stay in guest memory, never in command arguments, output or journals.
const CloudInitPasswordReadyScript = `def root_password_ready():
    try:
        import base64, hmac, os, pwd, yaml
        with open("/var/lib/cloud/instance/cloud-config.txt", encoding="utf-8") as stream:
            config = yaml.safe_load(stream)
        users = config.get("users", [])
        roots = [user for user in users if isinstance(user, dict) and user.get("name") == "root"]
        if len(roots) != 1 or roots[0].get("lock_passwd") is not False:
            return False
        expected = roots[0].get("hashed_passwd")
        if not isinstance(expected, str) or not (expected.startswith("$6$") or expected == "*"):
            return False
        keys = roots[0].get("ssh_authorized_keys", [])
        if not isinstance(keys, list) or any(not isinstance(key, str) for key in keys):
            return False
        if expected == "*" and not keys:
            return False
        if keys:
            def identity(line):
                fields = line.split()
                if len(fields) < 2 or fields[0] not in ("ssh-ed25519", "ssh-rsa", "ecdsa-sha2-nistp256", "ecdsa-sha2-nistp384", "ecdsa-sha2-nistp521"):
                    return None
                blob = base64.b64decode(fields[1], validate=True)
                return (fields[0], blob)
            expected_keys = {identity(key) for key in keys}
            if None in expected_keys:
                return False
            with open(os.path.join(pwd.getpwnam("root").pw_dir, ".ssh", "authorized_keys"), encoding="utf-8") as stream:
                applied = set()
                for line in stream:
                    try:
                        parsed = identity(line)
                    except (ValueError, TypeError):
                        continue
                    if parsed is not None:
                        applied.add(parsed)
            if not expected_keys.issubset(applied):
                return False
        with open("/etc/shadow", encoding="utf-8") as stream:
            for line in stream:
                fields = line.rstrip("\n").split(":")
                if fields[0] == "root":
                    return len(fields) >= 8 and (expected == "*" or fields[2] != "0") and hmac.compare_digest(fields[1], expected)
        return False
    except Exception:
        # YAML parser errors can include source lines containing a password hash.
        # Fail closed without printing parser details or any guest-local content.
        return False
`

// $1 is a fixed mode, never user input. Preserve cloud-init failures, including
// degraded status, and reject disabled/incomplete boots or unapplied passwords.
const CloudInitReadyCommand = `/usr/bin/cloud-init status --wait >/dev/null 2>&1 && test -f /var/lib/cloud/instance/boot-finished && /usr/bin/python3 -c '` + CloudInitPasswordReadyScript + `
import sys
sys.exit(0 if sys.argv[1:] == ["network"] or (sys.argv[1:] == ["login"] and root_password_ready()) else 1)
' "$1" >/dev/null 2>&1`

// Login completion has its own durable checkpoint, independent of an optional
// administrator template. Reboots neither reset passwords nor rerun cloud-init.
func workerLoginInitialize(ctx context.Context, a *platform.App, j platform.Job, r platform.Row) error {
	if (platform.Text(r["login_config_ciphertext"]) == "" && !platform.Bool(object(object(r["incus_config_snapshot"])["creation"])["cloud_init_capable"])) || !platform.Stamp(r["login_initialized_at"]).IsZero() {
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
		mode := "network"
		if platform.Text(r["login_config_ciphertext"]) != "" {
			mode = "login"
		}
		return a.Agent(platform.WithTaskDispatch(timeout, j, "login.cloud-ready"), j.NodeID, http.MethodPost, workerPath(r, "/exec"), platform.Row{
			// Fixed read-only checks compare guest-cached configuration and shadow;
			// no password/hash, caller input or guest output enters the journal.
			"command":     []string{"/usr/bin/timeout", "--signal=KILL", "600", "/bin/sh", "-c", CloudInitReadyCommand, "cloud-init-ready", mode},
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
