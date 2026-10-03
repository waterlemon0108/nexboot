package qemu

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
)

type fakeRunner struct {
	infoOut     []byte
	infoErr     error
	progress    string
	convertErr  error
	convertArgs []string
}

func (f *fakeRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	return f.infoOut, f.infoErr
}

func (f *fakeRunner) RunWithProgress(ctx context.Context, stdout io.Writer, name string, args ...string) error {
	f.convertArgs = append([]string{name}, args...)
	if f.progress != "" {
		_, _ = io.WriteString(stdout, f.progress)
	}
	return f.convertErr
}

func newTestClient(r Runner) *Client {
	return New(r, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func TestInfoParsesFormatAndSize(t *testing.T) {
	c := newTestClient(&fakeRunner{infoOut: []byte(`{"virtual-size":68719476736,"format":"vmdk"}`)})
	info, err := c.Info(context.Background(), "/imports/win.vmdk")
	if err != nil {
		t.Fatal(err)
	}
	if info.Format != "vmdk" || info.VirtualSize != 68719476736 {
		t.Fatalf("info = %#v", info)
	}
}

func TestInfoRejectsNonPositiveSize(t *testing.T) {
	c := newTestClient(&fakeRunner{infoOut: []byte(`{"virtual-size":0,"format":"raw"}`)})
	if _, err := c.Info(context.Background(), "/imports/x.raw"); err == nil {
		t.Fatal("expected error for zero virtual size")
	}
}

func TestInfoSurfacesRunnerError(t *testing.T) {
	c := newTestClient(&fakeRunner{infoErr: errors.New("boom")})
	if _, err := c.Info(context.Background(), "/imports/x.vmdk"); err == nil {
		t.Fatal("expected error")
	}
}

func TestConvertToRawBuildsArgsAndReportsProgress(t *testing.T) {
	r := &fakeRunner{progress: "    (0.00/100%)\r    (50.00/100%)\r    (99.90/100%)\r"}
	c := newTestClient(r)
	var pcts []int
	err := c.ConvertToRaw(context.Background(), "/imports/win.vmdk", "vmdk", "/dev/zvol/tank/win", func(p int) { pcts = append(pcts, p) })
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(r.convertArgs, " ")
	for _, want := range []string{"qemu-img", "convert", "-p", "-O raw", "-f vmdk", "/imports/win.vmdk", "/dev/zvol/tank/win"} {
		if !strings.Contains(got, want) {
			t.Fatalf("convert args %q missing %q", got, want)
		}
	}
	if len(pcts) == 0 || pcts[len(pcts)-1] != 100 {
		t.Fatalf("progress = %v, want final 100", pcts)
	}
}

func TestConvertToRawOmitsFormatFlagWhenEmpty(t *testing.T) {
	r := &fakeRunner{}
	c := newTestClient(r)
	if err := c.ConvertToRaw(context.Background(), "/imports/x.raw", "", "/dev/zvol/tank/x", nil); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(r.convertArgs, " "), "-f ") {
		t.Fatalf("did not expect -f flag: %v", r.convertArgs)
	}
}
