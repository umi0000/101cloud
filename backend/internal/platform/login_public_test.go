package platform

import "testing"

func TestPublicNeverSerializesInstanceLoginSecrets(t *testing.T) {
	row := Row{"id": uint64(1), "name": "kept", "login_config_ciphertext": []byte("encrypted-secret"), "root_password": "raw-secret", "password_hash": "$6$hash-secret", "ssh_public_keys": []string{"public-key-secret"}, "ssh_authorized_keys": []string{"public-key-secret"}}
	for _, result := range []Row{Public(row), Public(row, "id", "name", "login_config_ciphertext", "root_password", "password_hash", "ssh_public_keys", "ssh_authorized_keys")} {
		if result["id"] != "1" || result["name"] != "kept" {
			t.Fatal("public non-secret fields changed")
		}
		for _, field := range []string{"login_config_ciphertext", "root_password", "password_hash", "ssh_public_keys", "ssh_authorized_keys"} {
			if _, exists := result[field]; exists {
				t.Fatalf("public serializer disclosed %s", field)
			}
		}
	}
}
