package cni

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteConflist(t *testing.T) {
	dir := t.TempDir()
	err := WriteConflist(dir, "testnet", "cni0", "10.99.0.0/24", "10.99.0.0/16")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "10-tailscale-cni.conflist")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) == 0 {
		t.Fatal("empty conflist")
	}
	if !strings.Contains(string(data), "10.99.0.0/24") {
		t.Error("expected subnet in conflist")
	}
	if !strings.Contains(string(data), "10.99.0.0/16") {
		t.Error("expected cluster CIDR route in conflist")
	}
	if !strings.Contains(string(data), "bridge") {
		t.Error("expected bridge plugin")
	}
	if !strings.Contains(string(data), "portmap") {
		t.Error("expected portmap plugin")
	}
}

func TestGatewayFromSubnet(t *testing.T) {
	tests := []struct {
		subnet  string
		wantGW  string
	}{
		{"10.99.0.0/24", "10.99.0.1"},
		{"10.99.1.0/24", "10.99.1.1"},
		{"10.99.0.0/16", "10.99.0.1"},
	}
	for _, tt := range tests {
		got := gatewayFromSubnet(tt.subnet)
		if got != tt.wantGW {
			t.Errorf("gatewayFromSubnet(%q) = %q, want %q", tt.subnet, got, tt.wantGW)
		}
	}
}

func TestRemove(t *testing.T) {
	dir := t.TempDir()
	_ = WriteConflist(dir, "x", "cni0", "10.1.0.0/24", "")
	if err := Remove(dir); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "10-tailscale-cni.conflist")
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("expected file to be removed: %v", err)
	}
}

