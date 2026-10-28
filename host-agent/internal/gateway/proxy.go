package gateway

import (
	"context"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"path/filepath"
	"strings"
	"time"
	"unicode"
)

// NewProxy exposes only the Incus API on a fixed local Unix socket. The caller
// must protect this handler with TLSConfig and TokenAuth; socket access grants full Incus
// administrator privileges. This handler does not expose /agent APIs.
func NewProxy(socketPath string) (http.Handler, error) {
	if !filepath.IsAbs(socketPath) || strings.IndexByte(socketPath, 0) >= 0 || filepath.Clean(socketPath) != socketPath {
		return nil, errors.New("Incus socket must be an absolute, clean filesystem path")
	}
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	transport := &http.Transport{
		// Never consult proxy environment variables or dial a URL-supplied host.
		Proxy: nil,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, "unix", socketPath)
		},
		DisableCompression:  true,
		MaxIdleConns:        32,
		MaxIdleConnsPerHost: 32,
		IdleConnTimeout:     90 * time.Second,
	}
	proxy := &httputil.ReverseProxy{
		Transport:     transport,
		FlushInterval: -1,
		ErrorLog:      log.New(io.Discard, "", 0),
		Rewrite: func(request *httputil.ProxyRequest) {
			request.Out.URL.Scheme = "http"
			request.Out.URL.Host = "incus"
			request.Out.URL.User = nil
			request.Out.Host = "incus"
			stripIdentityHeaders(request.Out.Header)
		},
		ModifyResponse: func(response *http.Response) error {
			location := response.Header.Get("Location")
			if location == "" {
				return nil
			}
			u, err := url.Parse(location)
			if err != nil {
				return errors.New("invalid upstream redirect")
			}
			if strings.EqualFold(u.Hostname(), "incus") {
				// Relative redirects preserve the public TLS origin without
				// trusting a user-controlled Host or forwarded-host header.
				u.Scheme = ""
				u.Host = ""
				u.User = nil
				if u.Path == "" {
					u.Path = "/"
				}
				// A leading // would become a network-path redirect after
				// removing the origin. Reject ambiguous upstream redirects.
				if !unambiguousPath(u) {
					return errors.New("invalid upstream redirect path")
				}
				response.Header.Set("Location", u.String())
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, request *http.Request, _ error) {
			if request.Context().Err() != nil {
				return
			}
			http.Error(w, "Incus service unavailable", http.StatusBadGateway)
		},
	}
	return http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodConnect {
			http.Error(w, "CONNECT is not supported", http.StatusMethodNotAllowed)
			return
		}
		if request.URL == nil || request.URL.IsAbs() || request.URL.Host != "" || request.URL.User != nil || request.URL.Opaque != "" || request.URL.Fragment != "" || !validAPIPath(request.URL) {
			http.Error(w, "invalid Incus API request target", http.StatusBadRequest)
			return
		}
		proxy.ServeHTTP(w, request)
	}), nil
}

func validAPIPath(u *url.URL) bool {
	path := u.Path
	if path != "/" && path != "/1.0" && !strings.HasPrefix(path, "/1.0/") {
		return false
	}
	return unambiguousPath(u)
}

func unambiguousPath(u *url.URL) bool {
	path := u.Path
	if !strings.HasPrefix(path, "/") {
		return false
	}
	if strings.Contains(path, "//") || strings.ContainsRune(path, '\\') || strings.ContainsFunc(path, unicode.IsControl) {
		return false
	}
	for rest := path; rest != ""; {
		part, tail, _ := strings.Cut(rest, "/")
		if part == "." || part == ".." {
			return false
		}
		rest = tail
	}
	if u.RawPath != "" {
		decoded, err := url.PathUnescape(u.RawPath)
		if err != nil || decoded != path || u.EscapedPath() != u.RawPath {
			return false
		}
	}
	return true
}

func stripIdentityHeaders(headers http.Header) {
	for name := range headers {
		if strings.EqualFold(name, "Authorization") || strings.EqualFold(name, "Proxy-Authorization") || strings.EqualFold(name, "Cookie") || strings.EqualFold(name, "Forwarded") || strings.EqualFold(name, "Remote-User") ||
			headerPrefix(name, "X-Forwarded") || headerPrefix(name, "X-Incus-Forwarded") || headerPrefix(name, "X-LXD-Forwarded") ||
			headerPrefix(name, "X-Incus-OIDC") || headerPrefix(name, "X-LXD-OIDC") || strings.EqualFold(name, "X-Incus-Authenticated") || strings.EqualFold(name, "X-LXD-Authenticated") ||
			headerPrefix(name, "X-Remote-") || headerPrefix(name, "X-Auth-") || headerPrefix(name, "X-Authenticated-") ||
			headerPrefix(name, "X-SSL-") || headerPrefix(name, "SSL-Client-") || headerPrefix(name, "X-Client-Cert") {
			delete(headers, name)
		}
	}
}

func headerPrefix(name, prefix string) bool {
	return len(name) >= len(prefix) && strings.EqualFold(name[:len(prefix)], prefix)
}
