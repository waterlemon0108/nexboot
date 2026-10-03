package storage

import "testing"

func TestClientCloneNameNormalizesMAC(t *testing.T) {
	if got := ClientCloneName("aa:bb-cc.dd ee:ff"); got != "CLIENT-AABBCCDDEEFF" {
		t.Fatalf("clone name = %q", got)
	}
}

func TestClientDataCloneNameUsesLUN(t *testing.T) {
	if got := ClientDataCloneName("aa:bb:cc:dd:ee:ff", 2); got != "CLIENT-AABBCCDDEEFF-DATA-2" {
		t.Fatalf("data clone name = %q", got)
	}
}

func TestSuperClientCloneNameNormalizesMAC(t *testing.T) {
	if got := SuperClientCloneName("aa:bb:cc:dd:ee:ff"); got != "SCLIENT-AABBCCDDEEFF" {
		t.Fatalf("super clone name = %q", got)
	}
	if got := SuperClientDataCloneName("aa:bb:cc:dd:ee:ff", 1); got != "SCLIENT-AABBCCDDEEFF-DATA-1" {
		t.Fatalf("super data clone name = %q", got)
	}
}
