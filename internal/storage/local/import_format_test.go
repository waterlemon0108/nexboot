package local

import "testing"

func TestImportFileClassification(t *testing.T) {
	for _, p := range []string{
		"/imports/a.vmdk", "/imports/a.vhd", "/imports/a.vhdx",
		"/imports/a.qcow2", "/imports/a.vdi", "/imports/a.raw", "/imports/a.img",
	} {
		if !isDiskImageFile(p) {
			t.Errorf("%s should classify as a disk image", p)
		}
		if isZFSStreamFile(p) {
			t.Errorf("%s should not classify as a zfs stream", p)
		}
		if !isSupportedImportImageFile(p) {
			t.Errorf("%s should be supported", p)
		}
	}
	for _, p := range []string{"/imports/a.zfs", "/imports/a.gzip", "/imports/a.zfs.gz"} {
		if isDiskImageFile(p) {
			t.Errorf("%s should not classify as a disk image", p)
		}
		if !isZFSStreamFile(p) {
			t.Errorf("%s should classify as a zfs stream", p)
		}
		if !isSupportedImportImageFile(p) {
			t.Errorf("%s should be supported", p)
		}
	}
	for _, p := range []string{"/imports/a.iso", "/imports/a.txt"} {
		if isSupportedImportImageFile(p) {
			t.Errorf("%s should be unsupported", p)
		}
	}
}
