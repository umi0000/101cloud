package instances

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"101cloud/backend/internal/platform"
	cryptsha512 "github.com/sergeymakinen/go-crypt/sha512"
	"golang.org/x/crypto/ssh"
)

func TestLoginPasswordValidation(t *testing.T) {
	for _, password := range []string{"", "short", strings.Repeat("密", 129), "valid\x00password", "valid\npassword", "valid\x7fpassword", "invalid\xffpassword"} {
		if _, err := normalizeLogin(&LoginRequest{RootPassword: password}); err == nil {
			t.Fatal("invalid root password accepted")
		}
	}
	if got, err := normalizeLogin(nil); err != nil || got != nil {
		t.Fatal("omitted login must remain supported")
	}
	password := " 密码:[]$xyz "
	if got, err := normalizeLogin(&LoginRequest{RootPassword: password}); err != nil || got.RootPassword != password {
		t.Fatal("password normalization changed caller input")
	}
}

func testSSHPublicKey(t *testing.T) string {
	t.Helper()
	public, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	key, err := ssh.NewPublicKey(public)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key)))
}

func TestLoginPublicKeyNormalizationAndLimits(t *testing.T) {
	key := testSSHPublicKey(t)
	got, err := normalizeLogin(&LoginRequest{SSHPublicKeys: []string{key + " first comment", "  " + key + " second comment  "}})
	if err != nil || len(got.SSHPublicKeys) != 1 || got.SSHPublicKeys[0] != key {
		t.Fatal("public keys were not canonicalized and deduplicated")
	}
	if _, err := normalizeLogin(&LoginRequest{RootPassword: "valid password", SSHPublicKeys: []string{key}}); err == nil {
		t.Fatal("simultaneous password and key login accepted")
	}
	for _, entries := range [][]string{{""}, {"garbage"}, {"-----BEGIN OPENSSH PRIVATE KEY-----"}, {"command=\"false\" " + key}, {key + "\n" + key}, {key + "\x00private"}, {"ssh-ed25519 " + base64.StdEncoding.EncodeToString([]byte("invalid blob"))}, {strings.Repeat("x", 8193)}, {key + " " + strings.Repeat("密", 2800)}, make([]string, 11)} {
		if _, err := normalizeSSHPublicKeys(entries); err == nil || strings.Contains(err.Error(), "private") {
			t.Fatal("invalid public key accepted or validation echoed input")
		}
	}
	if _, err := normalizeSSHPublicKeys([]string{key + " " + strings.Repeat("x", 8192-len(key)-1)}); err != nil {
		t.Fatal("8192-byte public key entry rejected")
	}
	duplicates := make([]string, 11)
	for i := range duplicates {
		duplicates[i] = key
	}
	if _, err := normalizeSSHPublicKeys(duplicates); err == nil {
		t.Fatal("entry limit was checked only after deduplication")
	}
	if got, err := normalizeSSHPublicKeys(duplicates[:10]); err != nil || len(got) != 1 {
		t.Fatal("ten valid duplicate entries must be accepted and deduplicated")
	}
	if _, err := normalizeSSHPublicKeys([]string{strings.Replace(key, "ssh-ed25519", "ssh-rsa", 1)}); err == nil {
		t.Fatal("declared public key type mismatch accepted")
	}
	for _, bits := range []int{1024, 2048} {
		private, err := rsa.GenerateKey(rand.Reader, bits)
		if err != nil {
			t.Fatal(err)
		}
		public, err := ssh.NewPublicKey(&private.PublicKey)
		if err != nil {
			t.Fatal(err)
		}
		_, err = normalizeSSHPublicKeys([]string{strings.TrimSpace(string(ssh.MarshalAuthorizedKey(public)))})
		if (err == nil) != (bits >= 2048) {
			t.Fatal("RSA strength validation failed")
		}
	}
	for _, curve := range []elliptic.Curve{elliptic.P256(), elliptic.P384(), elliptic.P521()} {
		private, err := ecdsa.GenerateKey(curve, rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		public, err := ssh.NewPublicKey(&private.PublicKey)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := normalizeSSHPublicKeys([]string{strings.TrimSpace(string(ssh.MarshalAuthorizedKey(public)))}); err != nil {
			t.Fatal("valid ECDSA key rejected")
		}
	}
	public, _, _, _, err := ssh.ParseAuthorizedKey([]byte(key))
	if err != nil {
		t.Fatal(err)
	}
	certificate := &ssh.Certificate{Key: public, CertType: ssh.UserCert, ValidPrincipals: []string{"root"}}
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(private)
	if err != nil {
		t.Fatal(err)
	}
	if err := certificate.SignCert(rand.Reader, signer); err != nil {
		t.Fatal(err)
	}
	if _, err := normalizeSSHPublicKeys([]string{strings.TrimSpace(string(ssh.MarshalAuthorizedKey(certificate)))}); err == nil {
		t.Fatal("SSH certificate accepted as an ordinary public key")
	}
}

func assertMinimalCloudConfig(t *testing.T, data string, password bool) platform.Row {
	t.Helper()
	if !strings.HasPrefix(data, "#cloud-config\n") {
		t.Fatal("missing cloud-config header")
	}
	var cloud platform.Row
	if json.Unmarshal([]byte(strings.TrimPrefix(data, "#cloud-config\n")), &cloud) != nil {
		t.Fatal("cloud-config is not valid JSON/YAML")
	}
	if cloud["ssh_deletekeys"] != false || cloud["ssh_keys"] == nil || len(object(cloud["ssh_keys"])) != 0 || object(cloud["ssh"])["emit_keys_to_console"] != false || cloud["no_ssh_fingerprints"] != true || cloud["allow_public_ssh_keys"] != false || object(cloud["ssh_publish_hostkeys"])["enabled"] != false {
		t.Fatal("cloud-config does not suppress SSH generation, publication and fingerprints")
	}
	for _, forbidden := range []string{"ssh_pwauth", "ssh_genkeytypes", "ssh_authorized_keys", "write_files", "runcmd", "bootcmd", "packages", "package_update", "plain_text_passwd", "password", "cloud_init_modules", "cloud_config_modules", "cloud_final_modules"} {
		if _, ok := cloud[forbidden]; ok {
			t.Fatalf("unexpected cloud-config field %s", forbidden)
		}
	}
	want := 6
	if password {
		want += 2
		if _, supplied := object(rows(cloud["users"])[0])["ssh_authorized_keys"]; supplied {
			want++
			if cloud["disable_root"] != false {
				t.Fatal("root keys must not receive forced-command restrictions")
			}
		}
	} else if _, ok := cloud["users"]; ok {
		t.Fatal("network-only cloud-config modified accounts")
	}
	if len(cloud) != want {
		t.Fatal("cloud-config is not minimal")
	}
	return cloud
}

func TestLoginEncryptionAndCloudConfigPreserveCredentialBoundaries(t *testing.T) {
	a := platform.New(nil, platform.Config{MasterKey: bytes.Repeat([]byte{7}, 32)})
	password := " 空格密码:$xyz "
	sealed, err := sealLogin(a, &LoginRequest{RootPassword: password})
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(sealed, []byte(password)) {
		t.Fatal("unencrypted password persisted")
	}
	raw, err := a.Decrypt(sealed)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(raw)
	var cfg loginConfig
	if err = json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(cfg.PasswordHash, "$6$") || cryptsha512.Check(cfg.PasswordHash, password) != nil || cryptsha512.Check(cfg.PasswordHash, strings.TrimSpace(password)) == nil {
		t.Fatal("password not SHA512-crypt or original whitespace was lost")
	}
	if bytes.Contains(sealed, []byte(cfg.PasswordHash)) || bytes.Contains(raw, []byte(password)) {
		t.Fatal("password or password hash escaped the encrypted boundary")
	}
	other, err := sealLogin(a, &LoginRequest{RootPassword: password})
	if err != nil {
		t.Fatal(err)
	}
	otherRaw, err := a.Decrypt(other)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(otherRaw)
	var otherCfg loginConfig
	if json.Unmarshal(otherRaw, &otherCfg) != nil || otherCfg.PasswordHash == cfg.PasswordHash {
		t.Fatal("password hash salt reused")
	}
	data, err := loginUserData(a, platform.Row{"login_config_ciphertext": sealed})
	if err != nil || strings.Contains(data, password) {
		t.Fatal("cloud-config failed or contains plaintext password")
	}
	cloud := assertMinimalCloudConfig(t, data, true)
	root := object(rows(cloud["users"])[0])
	if len(rows(cloud["users"])) != 1 || len(root) != 3 || root["name"] != "root" || root["hashed_passwd"] != cfg.PasswordHash || root["lock_passwd"] != false || object(cloud["chpasswd"])["expire"] != false {
		t.Fatal("password-only cloud-config changed")
	}
	sealed[0] ^= 1
	if _, err = loginUserData(a, platform.Row{"login_config_ciphertext": sealed}); err == nil || strings.Contains(err.Error(), password) || strings.Contains(err.Error(), cfg.PasswordHash) {
		t.Fatal("tampered ciphertext accepted or error leaked credentials")
	}
}

func TestNetworkOnlyCloudConfigStillSuppressesSSH(t *testing.T) {
	data, err := loginUserData(nil, platform.Row{})
	if err != nil {
		t.Fatal(err)
	}
	assertMinimalCloudConfig(t, data, false)
}

func TestEncryptedKeyOnlyAndFrozenHistoricalLogin(t *testing.T) {
	a := platform.New(nil, platform.Config{MasterKey: bytes.Repeat([]byte{3}, 32)})
	key := testSSHPublicKey(t)
	passwordHash, err := cryptsha512.NewHash("legacy password", cryptsha512.DefaultRounds)
	if err != nil {
		t.Fatal(err)
	}
	for _, hash := range []string{"", passwordHash} {
		sealed, err := a.Encrypt([]byte(jsonText(platform.Row{"password_hash": hash, "ssh_public_keys": []string{key}})))
		if err != nil {
			t.Fatal(err)
		}
		if hash == "" {
			sealed, err = sealLogin(a, &LoginRequest{SSHPublicKeys: []string{key}})
			if err != nil || bytes.Contains(sealed, []byte(key)) {
				t.Fatal("key-only login did not encrypt its public keys")
			}
			raw, err := a.Decrypt(sealed)
			if err != nil {
				t.Fatal(err)
			}
			var cfg loginConfig
			err = json.Unmarshal(raw, &cfg)
			clear(raw)
			if err != nil || cfg.PasswordHash != "" || len(cfg.SSHPublicKeys) != 1 || cfg.SSHPublicKeys[0] != key {
				t.Fatal("key-only encrypted configuration contains a password or lost keys")
			}
		}
		data, err := loginUserData(a, platform.Row{"login_config_ciphertext": sealed})
		if err != nil {
			t.Fatal("encrypted key configuration failed")
		}
		cloud := assertMinimalCloudConfig(t, data, true)
		root := object(rows(cloud["users"])[0])
		expectedHash := hash
		if expectedHash == "" {
			expectedHash = "*"
		}
		if root["hashed_passwd"] != expectedHash || root["lock_passwd"] != false || jsonText(root["ssh_authorized_keys"]) != jsonText([]string{key}) {
			t.Fatal("key-only sentinel or frozen historical credentials changed")
		}
	}
}

func TestRootPasswordReadinessChecksGuestFilesWithoutOutput(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is required to exercise guest password readiness")
	}
	resolved, err := exec.CommandContext(t.Context(), python, "-c", "import sys; print(sys.executable)").Output()
	if err != nil {
		t.Fatal(err)
	}
	python = strings.TrimSpace(string(resolved))
	if err := exec.CommandContext(t.Context(), python, "-c", "import yaml").Run(); err != nil {
		t.Skip("guest cloud-init Python YAML dependency is required")
	}
	hash, err := cryptsha512.NewHash("guest-only password", cryptsha512.DefaultRounds)
	if err != nil {
		t.Fatal(err)
	}
	key := testSSHPublicKey(t)
	secondKey := testSSHPublicKey(t)
	for _, tc := range []struct {
		name, actual, changed, config string
		users                         []platform.Row
		keys                          []string
		authorized                    string
		want                          bool
	}{
		{name: "applied-no-ssh-tools", actual: hash, changed: "20000", want: true},
		{name: "key-only-ready", actual: "*", changed: "0", keys: []string{key}, authorized: key, users: []platform.Row{{"name": "root", "hashed_passwd": "*", "lock_passwd": false}}, want: true},
		{name: "key-only-key-missing", actual: "*", changed: "20000", keys: []string{key}, users: []platform.Row{{"name": "root", "hashed_passwd": "*", "lock_passwd": false}}},
		{name: "key-only-locked-account", actual: "!*", changed: "20000", keys: []string{key}, authorized: key, users: []platform.Row{{"name": "root", "hashed_passwd": "*", "lock_passwd": false}}},
		{name: "key-only-empty-password", actual: "", changed: "20000", keys: []string{key}, authorized: key, users: []platform.Row{{"name": "root", "hashed_passwd": "*", "lock_passwd": false}}},
		{name: "sentinel-without-keys", actual: "*", changed: "20000", users: []platform.Row{{"name": "root", "hashed_passwd": "*", "lock_passwd": false}}},
		{name: "keys-applied", actual: hash, changed: "20000", keys: []string{key}, authorized: key + " guest comment\n", want: true},
		{name: "all-keys-applied", actual: hash, changed: "20000", keys: []string{key, secondKey}, authorized: secondKey + "\n" + key + "\n", want: true},
		{name: "one-key-missing", actual: hash, changed: "20000", keys: []string{key, secondKey}, authorized: key},
		{name: "unrelated-malformed-lines", actual: hash, changed: "20000", keys: []string{key}, authorized: "ssh-ed25519 invalid-base64!!!\n# ignored\n" + key, want: true},
		{name: "malformed-key-config-no-output", actual: hash, changed: "20000", keys: []string{"ssh-ed25519 invalid-base64!!!"}, authorized: key},
		{name: "key-missing", actual: hash, changed: "20000", keys: []string{key}},
		{name: "key-wrong", actual: hash, changed: "20000", keys: []string{key}, authorized: testSSHPublicKey(t)},
		{name: "restricted-key-not-applied", actual: hash, changed: "20000", keys: []string{key}, authorized: "command=\"false\" " + key},
		{name: "key-applied-password-wrong", actual: "$6$wrong$hash", changed: "20000", keys: []string{key}, authorized: key},
		{name: "wrong-password", actual: "$6$another$hash", changed: "20000"},
		{name: "locked-password", actual: "!" + hash, changed: "20000"},
		{name: "forced-expiry", actual: hash, changed: "0"},
		{name: "missing-root-configuration", actual: hash, changed: "20000", users: []platform.Row{}},
		{name: "duplicate-root-configuration", actual: hash, changed: "20000", users: []platform.Row{{"name": "root"}, {"name": "root"}}},
		{name: "locked-configuration", actual: hash, changed: "20000", users: []platform.Row{{"name": "root", "hashed_passwd": hash, "lock_passwd": true}}},
		{name: "malformed-config-no-secret-traceback", actual: hash, changed: "20000", config: "users: [private-hash-must-not-escape\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			config, shadow := filepath.Join(dir, "cloud-config.txt"), filepath.Join(dir, "shadow")
			users := tc.users
			if users == nil {
				users = []platform.Row{{"name": "root", "hashed_passwd": hash, "lock_passwd": false}}
			}
			if len(tc.keys) != 0 {
				users[0]["ssh_authorized_keys"] = tc.keys
			}
			authorized := filepath.Join(dir, "authorized_keys")
			if tc.authorized != "" {
				if err := os.WriteFile(authorized, []byte(tc.authorized), 0600); err != nil {
					t.Fatal(err)
				}
			}
			contents := tc.config
			if contents == "" {
				contents = jsonText(platform.Row{"users": users})
			}
			if err := os.WriteFile(config, []byte(contents), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(shadow, []byte("root:"+tc.actual+":"+tc.changed+":0:99999:7:::\n"), 0600); err != nil {
				t.Fatal(err)
			}
			script := strings.ReplaceAll(CloudInitPasswordReadyScript, strconv.Quote("/var/lib/cloud/instance/cloud-config.txt"), strconv.Quote(config))
			script = strings.ReplaceAll(script, strconv.Quote("/etc/shadow"), strconv.Quote(shadow))
			script = strings.ReplaceAll(script, `os.path.join(pwd.getpwnam("root").pw_dir, ".ssh", "authorized_keys")`, strconv.Quote(authorized))
			cmd := exec.CommandContext(t.Context(), python, "-c", script+"\nimport sys\nsys.exit(0 if root_password_ready() else 1)\n")
			// Empty PATH proves the readiness verifier does not invoke SSH tools,
			// cloud-init, or any other external executable after Python starts.
			cmd.Env = append(os.Environ(), "PATH="+dir)
			output, err := cmd.CombinedOutput()
			if (err == nil) != tc.want || len(output) != 0 {
				t.Fatalf("unexpected readiness outcome or nonempty output: ready=%t output-bytes=%d", err == nil, len(output))
			}
		})
	}
}

// This exercises installed cloud-init modules, not a reimplementation of their
// branching. Guest filesystem writes are observed, while every subprocess,
// key deletion/import/publication and fingerprint attempt fails the test.
func TestCloudInitActualSSHModulesAcceptSuppressionWithoutTools(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is required for the actual cloud-init module regression")
	}
	if err := exec.CommandContext(t.Context(), python, "-c", "import cloudinit.config.schema, cloudinit.config.cc_ssh, cloudinit.config.cc_keys_to_console, cloudinit.config.cc_ssh_authkey_fingerprints").Run(); err != nil {
		t.Skip("installed guest cloud-init modules are required")
	}
	a := platform.New(nil, platform.Config{MasterKey: bytes.Repeat([]byte{4}, 32)})
	sealed, err := sealLogin(a, &LoginRequest{RootPassword: "module regression password"})
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range []platform.Row{{}, {"login_config_ciphertext": sealed}} {
		data, err := loginUserData(a, row)
		if err != nil {
			t.Fatal(err)
		}
		const script = `import copy, json, sys, types
from cloudinit import ssh_util, subp, util
from cloudinit.config import cc_ssh, cc_keys_to_console, cc_ssh_authkey_fingerprints
from cloudinit.config.schema import validate_cloudconfig_schema
config = json.load(sys.stdin)
validate_cloudconfig_schema(config, strict=True)
attempts = []
def forbidden(*args, **kwargs):
    attempts.append(True)
    raise AssertionError("unexpected SSH action")
subp.subp = forbidden
util.del_file = forbidden
ssh_util.extract_authorized_keys = forbidden
writes = []
ssh_util.setup_user_keys = lambda keys, user, **kwargs: writes.append((list(keys), user))
cloud = types.SimpleNamespace(distro=types.SimpleNamespace(get_default_user=lambda: {}, osfamily="arch"), get_public_ssh_keys=forbidden, datasource=types.SimpleNamespace(publish_host_keys=forbidden))
for module in (cc_ssh, cc_keys_to_console, cc_ssh_authkey_fingerprints):
    module.handle(module.__name__, copy.deepcopy(config), cloud, [])
assert not attempts
assert all(not keys and user == "root" for keys, user in writes)
`
		cmd := exec.CommandContext(t.Context(), python, "-c", script)
		cmd.Stdin = strings.NewReader(strings.TrimPrefix(data, "#cloud-config\n"))
		cmd.Env = append(os.Environ(), "PATH="+t.TempDir())
		output, err := cmd.CombinedOutput()
		if err != nil || len(output) != 0 {
			t.Fatalf("actual cloud-init schema/module regression failed or produced output: failed=%t output-bytes=%d", err != nil, len(output))
		}
	}
}
