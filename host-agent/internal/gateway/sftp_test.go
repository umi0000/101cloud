package gateway

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"testing"
	"time"

	"github.com/pkg/sftp"
)

// Incus file management upgrades HTTP directly to SFTP; there is no SSH
// connection or SSH daemon involved in this protocol.
func TestProxySFTPUpgradeTransfersFilesWithoutSSH(t *testing.T) {
	finished := make(chan error, 1)
	public, _ := unixGateway(t, http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Upgrade") != "sftp" {
			finished <- errors.New("SFTP upgrade header was not preserved")
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		connection, buffered, err := w.(http.Hijacker).Hijack()
		if err != nil {
			finished <- err
			return
		}
		defer connection.Close()
		_ = connection.SetDeadline(time.Now().Add(5 * time.Second))
		if _, err := buffered.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: sftp\r\n\r\n"); err != nil {
			finished <- err
			return
		}
		if err := buffered.Flush(); err != nil {
			finished <- err
			return
		}
		server, err := sftp.NewServer(connection)
		if err != nil {
			finished <- err
			return
		}
		defer server.Close()
		err = server.Serve()
		if errors.Is(err, io.EOF) {
			err = nil
		}
		finished <- err
	}))
	endpoint, err := url.Parse(public.URL)
	if err != nil {
		t.Fatal(err)
	}
	connection, err := net.DialTimeout("tcp", endpoint.Host, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	_ = connection.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := fmt.Fprintf(connection, "GET /1.0/instances/workload/sftp HTTP/1.1\r\nHost: %s\r\nConnection: Upgrade\r\nUpgrade: sftp\r\n\r\n", endpoint.Host); err != nil {
		t.Fatal(err)
	}
	buffered := bufio.NewReader(connection)
	response, err := http.ReadResponse(buffered, &http.Request{Method: http.MethodGet})
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusSwitchingProtocols || response.Header.Get("Upgrade") != "sftp" {
		t.Fatalf("SFTP upgrade failed: %s, Upgrade=%q", response.Status, response.Header.Get("Upgrade"))
	}
	client, err := sftp.NewClientPipe(buffered, connection)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	remotePath := filepath.Join(t.TempDir(), "transferred-file.bin")
	payload := []byte{'I', 'n', 'c', 'u', 's', 0, 0xff, '\n'}
	file, err := client.Create(remotePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write(payload); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	file, err = client.Open(remotePath)
	if err != nil {
		t.Fatal(err)
	}
	actual, err := io.ReadAll(file)
	_ = file.Close()
	if err != nil || !bytes.Equal(actual, payload) {
		t.Fatalf("SFTP downloaded file = %v, error = %v", actual, err)
	}
	if err := client.Remove(remotePath); err != nil {
		t.Fatal(err)
	}
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-finished:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("closing SFTP client did not close upgraded Unix connection")
	}
}

func TestProxyRewritesInternalLocationWithoutRestrictingRedirectPath(t *testing.T) {
	for _, test := range []struct {
		location string
		want     string
	}{
		{location: "http://incus/ui/assets/?version=1#entry", want: "/ui/assets/?version=1#entry"},
		{location: "/ui/", want: "/ui/"},
		{location: "https://images.example.com/redirect", want: "https://images.example.com/redirect"},
	} {
		t.Run(test.location, func(t *testing.T) {
			public, _ := unixGateway(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Location", test.location)
				w.WriteHeader(http.StatusAccepted)
			}))
			response, err := public.Client().Get(public.URL + "/1.0")
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			if response.StatusCode != http.StatusAccepted || response.Header.Get("Location") != test.want {
				t.Fatalf("redirect status=%d Location=%q, want %q", response.StatusCode, response.Header.Get("Location"), test.want)
			}
		})
	}
}
