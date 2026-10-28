package gateway

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
)

// LoadToken reads a node's secret from an owner-only regular file. A single
// trailing line ending is allowed for text-file tools; the token is canonical
// unpadded base64url encoding of exactly 32 random bytes.
func LoadToken(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", errors.New("cannot open node token")
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
		return "", errors.New("node token must be a regular file with mode 0600")
	}
	data, err := io.ReadAll(io.LimitReader(file, 47))
	if err != nil {
		return "", errors.New("cannot read node token")
	}
	token := string(data)
	if strings.HasSuffix(token, "\r\n") {
		token = strings.TrimSuffix(token, "\r\n")
	} else {
		token = strings.TrimSuffix(token, "\n")
	}
	if !validToken(token) {
		return "", errors.New("node token must encode exactly 32 bytes as unpadded base64url")
	}
	return token, nil
}

func validToken(token string) bool {
	if len(token) != 43 {
		return false
	}
	decoded, err := base64.RawURLEncoding.Strict().DecodeString(token)
	return err == nil && len(decoded) == 32
}

// TokenAuth accepts only one exact Bearer Authorization field over HTTPS.
// Tokens in cookies or URL parameters are never used as credentials. The
// credential is removed before proxying, including WebSocket upgrade requests.
func TokenAuth(token string, next http.Handler) http.Handler {
	expected := sha256.Sum256([]byte(token))
	configured := validToken(token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Count case-insensitively as well: real HTTP requests have canonical
		// keys, but handlers must also be safe with directly constructed ones.
		var authorization string
		count := 0
		for name, values := range r.Header {
			if strings.EqualFold(name, "Authorization") {
				count += len(values)
				if len(values) == 1 {
					authorization = values[0]
				}
			}
		}
		credential := strings.TrimPrefix(authorization, "Bearer ")
		actual := sha256.Sum256([]byte(credential))
		matches := subtle.ConstantTimeCompare(expected[:], actual[:]) == 1
		if !configured || r.TLS == nil || count != 1 || !strings.HasPrefix(authorization, "Bearer ") || !validToken(credential) || !matches {
			w.Header().Set("WWW-Authenticate", `Bearer realm="hostagent"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		for name := range r.Header {
			if strings.EqualFold(name, "Authorization") {
				delete(r.Header, name)
			}
		}
		next.ServeHTTP(w, r)
	})
}
