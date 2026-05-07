package bootstrap

import "testing"

func TestParseNetworkProbeFields(t *testing.T) {
	p := Parse("net_iface=wlan0\nnet_kind=wireless\nnet_ip=192.168.1.23\n")
	if p.NetworkInterface != "wlan0" {
		t.Fatalf("NetworkInterface = %q, want wlan0", p.NetworkInterface)
	}
	if p.NetworkKind != "wireless" {
		t.Fatalf("NetworkKind = %q, want wireless", p.NetworkKind)
	}
	if p.NetworkIP != "192.168.1.23" {
		t.Fatalf("NetworkIP = %q, want 192.168.1.23", p.NetworkIP)
	}
}
