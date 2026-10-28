package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"101cloud/hostagent/internal/config"
)

func TestInitThenCheckWithoutIncus(t *testing.T) {
	state := filepath.Join(t.TempDir(), "agent")
	var output, errors bytes.Buffer
	args := []string{"init", "--state-dir", state, "--endpoint", "https://node.example.test:8443", "--listen", "0.0.0.0:8443", "--incus-socket", "/nonexistent/incus.socket", "--validity", "2h"}
	if code := runArgs(args, &output, &errors); code != 0 {
		t.Fatalf("init failed (%d): %s", code, errors.String())
	}
	for _, name := range []string{"agent.json", "server.crt", "server.key", "token", "connection.json"} {
		if !strings.Contains(output.String(), filepath.Join(state, name)) {
			t.Fatalf("output omitted %s: %s", name, output.String())
		}
	}
	token, err := os.ReadFile(filepath.Join(state, "token"))
	if err != nil {
		t.Fatal(err)
	}
	key, err := os.ReadFile(filepath.Join(state, "server.key"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(output.String()+errors.String(), string(token)) || strings.Contains(output.String()+errors.String(), string(key)) || strings.Contains(output.String(), "PRIVATE KEY") {
		t.Fatal("init printed secret material")
	}
	cfg, err := config.Load(filepath.Join(state, "agent.json"))
	if err != nil || cfg.Listen != "0.0.0.0:8443" || cfg.IncusSocket != "/nonexistent/incus.socket" {
		t.Fatalf("flags were not applied: %+v, %v", cfg, err)
	}
	output.Reset()
	errors.Reset()
	if code := runArgs([]string{"-config", filepath.Join(state, "agent.json"), "-check"}, &output, &errors); code != 0 {
		t.Fatalf("offline check failed (%d): %s", code, errors.String())
	}
	if !strings.Contains(errors.String(), "Incus connectivity was not checked") {
		t.Fatalf("missing offline-check confirmation: %s", errors.String())
	}
	if err := os.WriteFile(filepath.Join(state, "token"), []byte("invalid"), 0600); err != nil {
		t.Fatal(err)
	}
	errors.Reset()
	if code := runArgs([]string{"-config", filepath.Join(state, "agent.json"), "-check"}, &output, &errors); code != 1 || !strings.Contains(errors.String(), "token rejected") {
		t.Fatalf("check did not reject bad token (%d): %s", code, errors.String())
	}
}

func TestInitDefaultsAndNoAutomaticRotation(t *testing.T) {
	state := filepath.Join(t.TempDir(), "agent")
	var output, errors bytes.Buffer
	args := []string{"init", "--state-dir", state, "--endpoint", "https://[::1]:9443"}
	if code := runArgs(args, &output, &errors); code != 0 {
		t.Fatalf("init failed (%d): %s", code, errors.String())
	}
	data, err := os.ReadFile(filepath.Join(state, "agent.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cfg config.Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Listen != "[::1]:9443" || cfg.TokenFile != "token" || cfg.TLS.Cert != "server.crt" || cfg.TLS.Key != "server.key" {
		t.Fatalf("unexpected generated defaults: %+v", cfg)
	}
	before, err := os.ReadFile(filepath.Join(state, "connection.json"))
	if err != nil {
		t.Fatal(err)
	}
	output.Reset()
	errors.Reset()
	if code := runArgs(args, &output, &errors); code == 0 {
		t.Fatal("init automatically replaced existing installation")
	}
	after, err := os.ReadFile(filepath.Join(state, "connection.json"))
	if err != nil || !bytes.Equal(before, after) || output.Len() != 0 {
		t.Fatalf("failed reinitialization changed connection or printed success: %v, %s", err, output.String())
	}
}

func TestCommandHelp(t *testing.T) {
	for _, args := range [][]string{{"--help"}, {"-h"}, {"help"}, {"init", "--help"}} {
		var output, errors bytes.Buffer
		if code := runArgs(args, &output, &errors); code != 0 {
			t.Fatalf("%v returned %d: %s", args, code, errors.String())
		}
		help := output.String() + errors.String()
		if !strings.Contains(help, "hostagent") || !strings.Contains(help, "endpoint") || !strings.Contains(help, "state-dir") {
			t.Fatalf("incomplete help for %v: %s", args, help)
		}
		if len(args) == 2 {
			for _, flag := range []string{"listen", "incus-socket", "validity", "8760h"} {
				if !strings.Contains(help, flag) {
					t.Fatalf("init help missing %s: %s", flag, help)
				}
			}
		} else if !strings.Contains(help, "-check") || !strings.Contains(help, "-config") {
			t.Fatalf("root help missing normal command flags: %s", help)
		}
	}
}

func TestCommandInputErrors(t *testing.T) {
	for _, args := range [][]string{
		{"--unknown"}, {"init-ca"}, {"issue-server"}, {"init"}, {"init", "--endpoint", "https://node:8443"},
		{"init", "--state-dir", "unused"}, {"init", "--unknown"}, {"init", "--validity", "one-year"},
		{"-check", "extra"}, {"init", "extra"}, {"init", "--cert", "ca.crt"},
	} {
		var output, errors bytes.Buffer
		if code := runArgs(args, &output, &errors); code != 2 {
			t.Fatalf("%v returned %d instead of usage error: %s", args, code, errors.String())
		}
		if output.Len() != 0 || errors.Len() == 0 {
			t.Fatalf("input error did not use stderr for %v", args)
		}
	}
}

func TestInitBadEndpointDoesNotExposeURLTokenOrCreateFiles(t *testing.T) {
	parent := t.TempDir()
	state := filepath.Join(parent, "agent")
	var output, errors bytes.Buffer
	if code := runArgs([]string{"init", "--state-dir", state, "--endpoint", "https://node:8443?token=do-not-print-this"}, &output, &errors); code == 0 {
		t.Fatal("URL token query accepted")
	}
	if strings.Contains(output.String()+errors.String(), "do-not-print-this") {
		t.Fatal("endpoint rejection exposed URL token")
	}
	entries, err := os.ReadDir(parent)
	if err != nil || len(entries) != 0 {
		t.Fatalf("invalid init left state behind: %v, %v", entries, err)
	}
}

func TestCheckRejectsBadCertificate(t *testing.T) {
	state := filepath.Join(t.TempDir(), "agent")
	var output, errors bytes.Buffer
	if code := runArgs([]string{"init", "--state-dir", state, "--endpoint", "https://node:8443"}, &output, &errors); code != 0 {
		t.Fatalf("init failed: %s", errors.String())
	}
	if err := os.WriteFile(filepath.Join(state, "server.crt"), []byte("invalid"), 0600); err != nil {
		t.Fatal(err)
	}
	errors.Reset()
	if code := runArgs([]string{"-config", filepath.Join(state, "agent.json"), "-check"}, &output, &errors); code != 1 || !strings.Contains(errors.String(), "TLS configuration rejected") {
		t.Fatalf("check accepted invalid certificate (%d): %s", code, errors.String())
	}
}
