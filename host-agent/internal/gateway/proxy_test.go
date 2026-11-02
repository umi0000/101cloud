package gateway

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func unixGateway(t *testing.T, backend http.Handler) (*httptest.Server, string) {
	t.Helper()
	// Keep the path below the Unix-domain socket limit even on macOS.
	dir, err := os.MkdirTemp("", "gateway-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := filepath.Join(dir, "incus.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: backend}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })
	proxy, err := NewProxy(socket)
	if err != nil {
		t.Fatal(err)
	}
	public := httptest.NewServer(proxy)
	t.Cleanup(public.Close)
	return public, socket
}

func TestProxyRejectsUnsafeTargetsBeforeDial(t *testing.T) {
	proxy, err := NewProxy(filepath.Join(t.TempDir(), "absent.sock"))
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{
		"http://example.com/1.0", "//example.com/1.0", "/agent/v1/node", "/1.00", "/1.0evil",
		"/1.0//instances", "/1.0/../agent/v1/node", "/1.0/./instances", "/1.0/%2e%2e/instances",
		"/1.0/%2Finstances", "/1.0/%5cinstances", "/1.0/instances%00", "/1.0/instances%0a",
	} {
		t.Run(target, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, target, nil)
			// NewRequest uses an absolute URL as the in-memory target even
			// though a real origin-form server request has no scheme/host.
			if !strings.HasPrefix(target, "http:") {
				request.URL.Scheme = ""
				request.URL.Host = ""
			}
			recorder := httptest.NewRecorder()
			proxy.ServeHTTP(recorder, request)
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("got status %d, want 400", recorder.Code)
			}
		})
	}
	request := httptest.NewRequest(http.MethodConnect, "/1.0", nil)
	recorder := httptest.NewRecorder()
	proxy.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("CONNECT status = %d", recorder.Code)
	}
}

func TestProxyRejectsInconsistentRawPath(t *testing.T) {
	proxy, err := NewProxy(filepath.Join(t.TempDir(), "absent.sock"))
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/1.0/instances", nil)
	request.URL.RawPath = "/1.0/%zz"
	recorder := httptest.NewRecorder()
	proxy.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("invalid RawPath status = %d", recorder.Code)
	}
}

func TestProxyIdentityBoundaryAndInternalRedirect(t *testing.T) {
	identities := []string{
		"Authorization", "Proxy-Authorization", "Cookie", "Forwarded", "X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Custom",
		"X-Incus-Forwarded-Username", "X-Incus-Forwarded-Protocol", "X-LXD-Forwarded-Address", "X-Incus-authenticated",
		"X-Incus-OIDC-Token", "X-Remote-User", "X-Auth-Request-User", "X-Authenticated-User", "X-SSL-Client-Cert", "SSL-Client-S-DN", "X-Client-Cert", "Remote-User",
	}
	observed := make(chan error, 1)
	public, _ := unixGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, header := range identities {
			if r.Header.Get(header) != "" {
				observed <- fmt.Errorf("identity header %s crossed the trust boundary", header)
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
		}
		if r.Host != "incus" {
			observed <- fmt.Errorf("upstream Host = %q", r.Host)
		} else {
			observed <- nil
		}
		w.Header().Set("Location", "http://incus/1.0/operations/42?project=tenant")
		w.WriteHeader(http.StatusAccepted)
	}))
	request, err := http.NewRequest(http.MethodPost, public.URL+"/1.0/instances", strings.NewReader(`{"name":"workload"}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Host = "attacker.invalid"
	for _, header := range identities {
		request.Header.Set(header, "untrusted-identity")
	}
	response, err := public.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if err := <-observed; err != nil {
		t.Fatal(err)
	}
	if got := response.Header.Get("Location"); got != "/1.0/operations/42?project=tenant" {
		t.Fatalf("Location = %q", got)
	}
}

func TestProxyStreamsBeforeCompletionAndCancelsUpstream(t *testing.T) {
	cancelled := make(chan struct{})
	public, _ := unixGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"operation\"}\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(cancelled)
	}))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, public.URL+"/1.0/events?type=operation", nil)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Timeout: 5 * time.Second}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	line, err := bufio.NewReader(response.Body).ReadString('\n')
	if err != nil || line != "data: {\"type\":\"operation\"}\n" {
		t.Fatalf("first live event = %q, error = %v", line, err)
	}
	cancel()
	select {
	case <-cancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("client cancellation did not reach Unix-socket upstream")
	}
}

func TestProxyWebSocketBidirectionalIncusOperation(t *testing.T) {
	finished := make(chan error, 1)
	public, _ := unixGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		connection, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			finished <- err
			return
		}
		defer connection.Close()
		_ = connection.SetReadDeadline(time.Now().Add(5 * time.Second))
		_ = connection.SetWriteDeadline(time.Now().Add(5 * time.Second))
		if err := connection.WriteJSON(map[string]string{"type": "operation", "status": "Running"}); err != nil {
			finished <- err
			return
		}
		kind, input, err := connection.ReadMessage()
		if err != nil {
			finished <- err
			return
		}
		if kind != websocket.BinaryMessage || string(input) != "exit\n" {
			finished <- fmt.Errorf("unexpected exec input kind=%d data=%q", kind, input)
			return
		}
		if err := connection.WriteMessage(websocket.BinaryMessage, []byte("process exited with status 0\n")); err != nil {
			finished <- err
			return
		}
		finished <- connection.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, "completed"), time.Now().Add(5*time.Second))
	}))
	endpoint := "ws" + strings.TrimPrefix(public.URL, "http") + "/1.0/operations/42/websocket?secret=operation-secret"
	connection, response, err := (&websocket.Dialer{HandshakeTimeout: 5 * time.Second}).Dial(endpoint, nil)
	if err != nil {
		if response != nil {
			_ = response.Body.Close()
		}
		t.Fatal(err)
	}
	defer connection.Close()
	_ = connection.SetReadDeadline(time.Now().Add(5 * time.Second))
	var event map[string]string
	if err := connection.ReadJSON(&event); err != nil || event["status"] != "Running" {
		t.Fatalf("initial operation event = %v, error = %v", event, err)
	}
	if err := connection.WriteMessage(websocket.BinaryMessage, []byte("exit\n")); err != nil {
		t.Fatal(err)
	}
	kind, output, err := connection.ReadMessage()
	if err != nil || kind != websocket.BinaryMessage || string(output) != "process exited with status 0\n" {
		t.Fatalf("exec result kind=%d data=%q error=%v", kind, output, err)
	}
	_, _, err = connection.ReadMessage()
	if !websocket.IsCloseError(err, websocket.CloseNormalClosure) {
		t.Fatalf("close handshake: %v", err)
	}
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
}

func TestProxyFailureDoesNotDiscloseSocketOrOperationSecret(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "private-incus.sock")
	proxy, err := NewProxy(socket)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/1.0/operations/42/websocket?secret=private-operation-secret", nil)
	recorder := httptest.NewRecorder()
	proxy.ServeHTTP(recorder, request)
	body := recorder.Body.String()
	if recorder.Code != http.StatusBadGateway || strings.Contains(body, socket) || strings.Contains(body, "private-operation-secret") {
		t.Fatalf("unsafe gateway error: status=%d body=%q", recorder.Code, body)
	}
}

func TestProxyRejectsUnsafeSocketConfiguration(t *testing.T) {
	for _, socket := range []string{"", "relative.sock", "/tmp/../incus.sock", "/tmp/incus\x00.sock"} {
		if _, err := NewProxy(socket); err == nil {
			t.Errorf("accepted unsafe socket path %q", socket)
		}
	}
}

func TestProxyAllowsEscapedSlashInImageAlias(t *testing.T) {
	public, _ := unixGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.EscapedPath() != "/1.0/images/aliases/ubuntu%2F24.04" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"type":"sync","status_code":200,"metadata":{"name":"ubuntu/24.04","target":"image-fingerprint"}}`)
	}))
	response, err := public.Client().Get(public.URL + "/1.0/images/aliases/ubuntu%2F24.04")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK || !strings.Contains(string(body), `"target":"image-fingerprint"`) {
		t.Fatalf("valid image alias inaccessible: status=%d body=%s", response.StatusCode, body)
	}
}

func TestProxyPreservesPinnedImagePullAndVerification(t *testing.T) {
	fingerprint := strings.Repeat("a", 64)
	payload := `{"source":{"type":"image","mode":"pull","server":"https://images.linuxcontainers.org","protocol":"simplestreams","fingerprint":"` + fingerprint + `"},"public":false}`
	operation := `{"type":"async","status_code":100,"operation":"/1.0/operations/image-download","metadata":{"id":"image-download","status":"Running"}}`
	public, _ := unixGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.RawQuery != "project=images-project" {
			t.Errorf("lost project scope: %s", r.URL.String())
			http.Error(w, "project missing", 400)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/1.0/images":
			body, err := io.ReadAll(r.Body)
			if err != nil || string(body) != payload {
				t.Errorf("altered pinned pull: %s error=%v", body, err)
				http.Error(w, "invalid pull", 400)
				return
			}
			w.WriteHeader(http.StatusAccepted)
			_, _ = io.WriteString(w, operation)
		case r.Method == http.MethodGet && r.URL.Path == "/1.0/images/"+fingerprint:
			_, _ = io.WriteString(w, `{"type":"sync","status_code":200,"metadata":{"fingerprint":"`+fingerprint+`","architecture":"x86_64","type":"container"}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	response, err := public.Client().Post(public.URL+"/1.0/images?project=images-project", "application/json", strings.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if err != nil || response.StatusCode != http.StatusAccepted || string(body) != operation {
		t.Fatalf("pull operation lost: status=%d body=%s error=%v", response.StatusCode, body, err)
	}
	response, err = public.Client().Get(public.URL + "/1.0/images/" + fingerprint + "?project=images-project")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err = io.ReadAll(response.Body)
	if err != nil || response.StatusCode != http.StatusOK || !strings.Contains(string(body), `"fingerprint":"`+fingerprint+`"`) {
		t.Fatalf("exact image verification unavailable: status=%d body=%s error=%v", response.StatusCode, body, err)
	}
}
