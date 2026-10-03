package tftpassets

import (
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

func TestWriteTo(t *testing.T) {
	dir := t.TempDir()
	nested := filepath.Join(dir, "nested", "tftp")

	if err := WriteTo(nested); err != nil {
		t.Fatalf("WriteTo: %v", err)
	}

	for _, name := range Names {
		want, err := fs.ReadFile(files, "files/"+name)
		if err != nil {
			t.Fatalf("read embedded %s: %v", name, err)
		}
		got, err := os.ReadFile(filepath.Join(nested, name))
		if err != nil {
			t.Fatalf("read written %s: %v", name, err)
		}
		if string(got) != string(want) {
			t.Errorf("%s: written content does not match embedded content", name)
		}
	}
}

func TestWriteTo_overwritesExisting(t *testing.T) {
	dir := t.TempDir()
	stray := filepath.Join(dir, "snponly.efi")
	if err := os.WriteFile(stray, []byte("stray distro-provided file"), 0o644); err != nil {
		t.Fatalf("seed stray file: %v", err)
	}

	if err := WriteTo(dir); err != nil {
		t.Fatalf("WriteTo: %v", err)
	}

	want, err := fs.ReadFile(files, "files/snponly.efi")
	if err != nil {
		t.Fatalf("read embedded snponly.efi: %v", err)
	}
	got, err := os.ReadFile(stray)
	if err != nil {
		t.Fatalf("read %s: %v", stray, err)
	}
	if string(got) != string(want) {
		t.Errorf("stray file was not overwritten with the embedded copy")
	}
}
