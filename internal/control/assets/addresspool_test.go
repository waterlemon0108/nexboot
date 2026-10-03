package assets

import (
	"errors"
	"testing"
)

func TestGroupAddressPoolBounds(t *testing.T) {
	pool, err := groupAddressPool("192.168.50.10", 3)
	if err != nil {
		t.Fatal(err)
	}
	// 窗口为 .10 .11 .12，两端都包含
	for ip, want := range map[uint32]bool{
		ipMustUint32(t, "192.168.50.9"):  false,
		ipMustUint32(t, "192.168.50.10"): true,
		ipMustUint32(t, "192.168.50.12"): true,
		ipMustUint32(t, "192.168.50.13"): false,
	} {
		if pool.contains(ip) != want {
			t.Fatalf("contains(%d) != %v", ip, want)
		}
	}
}

func TestGroupAddressPoolRejectsBadWindows(t *testing.T) {
	for name, tc := range map[string]struct {
		start string
		max   int
	}{
		"zero size":           {"192.168.50.10", 0},
		"negative size":       {"192.168.50.10", -1},
		"not ipv4":            {"fe80::1", 10},
		"garbage":             {"nope", 10},
		"wraps address space": {"255.255.255.250", 10},
	} {
		if _, err := groupAddressPool(tc.start, tc.max); !errors.Is(err, ErrGroupInvalidNetwork) {
			t.Fatalf("%s: err = %v, want ErrGroupInvalidNetwork", name, err)
		}
	}
}

func TestAddressPoolAllocateSkipsOccupied(t *testing.T) {
	pool, err := groupAddressPool("10.0.0.1", 3)
	if err != nil {
		t.Fatal(err)
	}
	occupied := map[uint32]bool{
		ipMustUint32(t, "10.0.0.1"): true,
		ipMustUint32(t, "10.0.0.2"): true,
	}
	ip, ok := pool.allocate(occupied)
	if !ok || ip != "10.0.0.3" {
		t.Fatalf("allocate = %q, %v", ip, ok)
	}
	occupied[ipMustUint32(t, "10.0.0.3")] = true
	if ip, ok := pool.allocate(occupied); ok {
		t.Fatalf("full window still allocated %q", ip)
	}
}

func ipMustUint32(t *testing.T, s string) uint32 {
	t.Helper()
	addr, err := parseRequiredIPv4(s)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return ipv4Uint32(addr)
}
