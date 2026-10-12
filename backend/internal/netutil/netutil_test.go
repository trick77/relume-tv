package netutil

import (
	"net"
	"testing"
)

func TestInterfaceForIP_InvalidIP(t *testing.T) {
	if _, err := InterfaceForIP("not-an-ip"); err == nil {
		t.Fatal("expected error for invalid IP, got nil")
	}
}

func TestInterfaceForIP_EmptyIP(t *testing.T) {
	if _, err := InterfaceForIP(""); err == nil {
		t.Fatal("expected error for empty IP, got nil")
	}
}

func TestPrimaryMAC_PicksLowestIndexPhysicalUpInterface(t *testing.T) {
	mac := func(s string) net.HardwareAddr { m, _ := net.ParseMAC(s); return m }
	ifaces := []net.Interface{
		{Index: 1, Name: "lo", Flags: net.FlagUp | net.FlagLoopback},
		{Index: 5, Name: "docker0", Flags: net.FlagUp, HardwareAddr: mac("02:42:ac:11:00:01")},
		{Index: 3, Name: "eth1", Flags: net.FlagUp, HardwareAddr: mac("aa:bb:cc:dd:ee:03")},
		{Index: 2, Name: "eth0", Flags: 0, HardwareAddr: mac("aa:bb:cc:dd:ee:02")}, // down
		{Index: 4, Name: "veth1234", Flags: net.FlagUp, HardwareAddr: mac("aa:bb:cc:dd:ee:04")},
	}
	got, ok := primaryMAC(ifaces)
	if !ok || got.String() != "aa:bb:cc:dd:ee:03" {
		t.Fatalf("primaryMAC = %v, %v; want eth1", got, ok)
	}
}

func TestPrimaryMAC_NoneEligible(t *testing.T) {
	ifaces := []net.Interface{
		{Index: 1, Name: "lo", Flags: net.FlagUp | net.FlagLoopback},
		{Index: 2, Name: "docker0", Flags: net.FlagUp, HardwareAddr: net.HardwareAddr{2, 0x42, 0, 0, 0, 1}},
	}
	if _, ok := primaryMAC(ifaces); ok {
		t.Fatal("expected no eligible interface")
	}
}
