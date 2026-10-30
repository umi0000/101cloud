package monitoring

import (
	"io"
	"net"
	"testing"
	"time"

	"101cloud/backend/internal/platform"
	"github.com/gin-gonic/gin"
)

func TestIncusTerminalEOFBarrierAcknowledged(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()
	_ = client.SetDeadline(time.Now().Add(3 * time.Second))
	_ = server.SetDeadline(time.Now().Add(3 * time.Second))
	bridge := &consoleBridge{data: &consoleWire{body: client}, frames: make(chan consoleFrame, 4), done: make(chan struct{})}
	received := make(chan error, 1)
	go func() {
		if _, e := server.Write([]byte{0x82, 3, 'o', 'u', 't', 0x81, 0}); e != nil {
			received <- e
			return
		}
		var header [2]byte
		if _, e := io.ReadFull(server, header[:]); e != nil {
			received <- e
			return
		}
		if header[0] != 0x81 || header[1] != 0x80 {
			received <- io.ErrUnexpectedEOF
			return
		}
		var mask [4]byte
		_, e := io.ReadFull(server, mask[:])
		received <- e
	}()
	go (&consoleService{}).readData(bridge)
	output := <-bridge.frames
	if string(output.data) != "out" || output.err != nil {
		t.Fatalf("output lost before EOF: %+v", output)
	}
	eof := <-bridge.frames
	if eof.err != io.EOF {
		t.Fatalf("Incus text barrier reported a protocol error: %v", eof.err)
	}
	if e := <-received; e != nil {
		t.Fatalf("stdin EOF barrier was not sent: %v", e)
	}
	if _, ok := <-bridge.frames; ok {
		t.Fatal("terminal did not close its output channel")
	}
}

func TestConsolePublicHasNoFixedExpiryAndPreservesHistory(t *testing.T) {
	s := &consoleService{a: platform.New(nil, platform.Config{})}
	c, _ := gin.CreateTestContext(nil)
	for _, status := range []string{"preparing", "ready", "active", "closed", "failed", "expired"} {
		row := consoleSession{ID: "console-ticket", Kind: "terminal", Status: status, Revision: 7, CreatedAt: s.a.Time().Add(-24 * time.Hour)}
		response := s.public(c, row).(map[string]any)
		if response["status"] != status {
			t.Fatalf("stored %s status was rewritten: %v", status, response)
		}
		if _, exists := response["expires_at"]; exists {
			t.Fatal("console response advertises a fixed expiry")
		}
		if response["protocol"] != "101cloud.terminal.v1" || response["revision"] != "7" {
			t.Fatalf("console protocol or revision changed: %v", response)
		}
	}
}
