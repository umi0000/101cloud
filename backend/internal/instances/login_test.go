package instances

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"strings"
	"testing"

	"101cloud/backend/internal/platform"
	cryptsha512 "github.com/sergeymakinen/go-crypt/sha512"
	"golang.org/x/crypto/ssh"
)

func loginTestKey(t *testing.T) string {
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

func TestLoginValidationRejectsUnsafeInputs(t *testing.T) {
	key := loginTestKey(t)
	weak, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatal(err)
	}
	weakKey, err := ssh.NewPublicKey(&weak.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		in   LoginRequest
	}{
		{"empty", LoginRequest{}},
		{"short-password", LoginRequest{RootPassword: "short"}},
		{"long-password", LoginRequest{RootPassword: strings.Repeat("密", 129)}},
		{"password-nul", LoginRequest{RootPassword: "valid\x00password"}},
		{"password-newline", LoginRequest{RootPassword: "valid\npassword"}},
		{"password-delete", LoginRequest{RootPassword: "valid\x7fpassword"}},
		{"empty-key", LoginRequest{SSHPublicKeys: []string{""}}},
		{"private-key", LoginRequest{SSHPublicKeys: []string{"-----BEGIN OPENSSH PRIVATE KEY-----"}}},
		{"options", LoginRequest{SSHPublicKeys: []string{`command="touch /tmp/pwned" ` + key}}},
		{"environment-options", LoginRequest{SSHPublicKeys: []string{`environment="X=1" ` + key}}},
		{"multiline", LoginRequest{SSHPublicKeys: []string{key + "\nruncmd: [evil]"}}},
		{"carriage-return", LoginRequest{SSHPublicKeys: []string{key + "\r"}}},
		{"oversized", LoginRequest{SSHPublicKeys: []string{key + strings.Repeat("x", 8192)}}},
		{"too-many", LoginRequest{SSHPublicKeys: []string{key, key, key, key, key, key, key, key, key, key, key}}},
		{"malformed-blob", LoginRequest{SSHPublicKeys: []string{"ssh-ed25519 AAAA"}}},
		{"weak-rsa", LoginRequest{SSHPublicKeys: []string{strings.TrimSpace(string(ssh.MarshalAuthorizedKey(weakKey)))}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := normalizeLogin(&tc.in); err == nil {
				t.Fatal("unsafe login input accepted")
			}
		})
	}
	if got, err := normalizeLogin(nil); err != nil || got != nil {
		t.Fatal("legacy missing-login behavior changed")
	}
}

func TestLoginNormalizesKeysWithoutYAMLOrShellInjection(t *testing.T) {
	key := loginTestKey(t)
	in := &LoginRequest{RootPassword: " 密码:[]$xyz ", SSHPublicKeys: []string{"  " + key + " runcmd: [touch, /tmp/pwned]", key + " another-comment"}}
	got, err := normalizeLogin(in)
	if err != nil || got.RootPassword != in.RootPassword || len(got.SSHPublicKeys) != 1 || got.SSHPublicKeys[0] != key {
		t.Fatal("normalization changed password, retained comments, or failed deduplication")
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
		if _, err = normalizeLogin(&LoginRequest{SSHPublicKeys: []string{strings.TrimSpace(string(ssh.MarshalAuthorizedKey(public)))}}); err != nil {
			t.Fatal("supported ECDSA key rejected")
		}
	}
	private, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	public, err := ssh.NewPublicKey(&private.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = normalizeLogin(&LoginRequest{SSHPublicKeys: []string{strings.TrimSpace(string(ssh.MarshalAuthorizedKey(public)))}}); err != nil {
		t.Fatal("supported RSA key rejected")
	}
}

func TestLoginEncryptionAndCloudConfigPreserveCredentialBoundaries(t *testing.T) {
	a := platform.New(nil, platform.Config{MasterKey: bytes.Repeat([]byte{7}, 32)})
	password := " 空格密码:$xyz "
	key := loginTestKey(t)
	sealed, err := sealLogin(a, &LoginRequest{RootPassword: password, SSHPublicKeys: []string{key}})
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(sealed, []byte(password)) || bytes.Contains(sealed, []byte(key)) {
		t.Fatal("unencrypted credentials persisted")
	}
	raw, err := a.Decrypt(sealed)
	if err != nil {
		t.Fatal(err)
	}
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
	var otherCfg loginConfig
	if json.Unmarshal(otherRaw, &otherCfg) != nil || otherCfg.PasswordHash == cfg.PasswordHash {
		t.Fatal("password hash salt reused")
	}
	data, err := loginUserData(a, platform.Row{"login_config_ciphertext": sealed})
	if err != nil || !strings.HasPrefix(data, "#cloud-config\n") || strings.Contains(data, password) {
		t.Fatal("cloud-config missing header or contains plaintext password")
	}
	var cloud map[string]any
	if json.Unmarshal([]byte(strings.TrimPrefix(data, "#cloud-config\n")), &cloud) != nil {
		t.Fatal("cloud-config is not valid JSON/YAML")
	}
	root := object(rows(cloud["users"])[0])
	marker := object(rows(cloud["write_files"])[0])
	if root["name"] != "root" || root["hashed_passwd"] != cfg.PasswordHash || root["lock_passwd"] != false || rows(root["ssh_authorized_keys"])[0] != key || cloud["disable_root"] != false || cloud["ssh_pwauth"] != true || object(cloud["chpasswd"])["expire"] != false || marker["path"] != "/etc/101cloud/ssh-login-mode" || marker["content"] != "password\n" || marker["owner"] != "root:root" || marker["permissions"] != "0600" {
		t.Fatal("cloud-init login schema or marker is incorrect")
	}
	for _, forbidden := range []string{"runcmd", "bootcmd", "packages", "package_update", "plain_text_passwd", "password"} {
		if _, ok := cloud[forbidden]; ok {
			t.Fatal("unexpected executable or plaintext cloud-config field")
		}
	}
	sealed[0] ^= 1
	if _, err = loginUserData(a, platform.Row{"login_config_ciphertext": sealed}); err == nil || strings.Contains(err.Error(), password) || strings.Contains(err.Error(), cfg.PasswordHash) {
		t.Fatal("tampered ciphertext accepted or error leaked credentials")
	}
}

func TestLoginKeyOnlyAccountIsUnusableForPasswordsNotLocked(t *testing.T) {
	a := platform.New(nil, platform.Config{MasterKey: bytes.Repeat([]byte{3}, 32)})
	sealed, err := sealLogin(a, &LoginRequest{SSHPublicKeys: []string{loginTestKey(t)}})
	if err != nil {
		t.Fatal(err)
	}
	data, err := loginUserData(a, platform.Row{"login_config_ciphertext": sealed})
	if err != nil {
		t.Fatal(err)
	}
	var cloud map[string]any
	if json.Unmarshal([]byte(strings.TrimPrefix(data, "#cloud-config\n")), &cloud) != nil {
		t.Fatal("invalid cloud-config")
	}
	root := object(rows(cloud["users"])[0])
	if root["hashed_passwd"] != "*" || root["lock_passwd"] != false || cloud["ssh_pwauth"] != false || object(rows(cloud["write_files"])[0])["content"] != "ssh-key\n" {
		t.Fatal("key-only account could accept passwords or is locked against public keys")
	}
	if data, err := loginUserData(a, platform.Row{}); err != nil || data != "" {
		t.Fatal("legacy instance gained login configuration")
	}
}
