package driverinf

import (
	"bytes"
	"reflect"
	"testing"
	"unicode/utf16"

	"golang.org/x/text/encoding/simplifiedchinese"
)

const sampleINF = `; Intel e1000e style sample
[Version]
Signature   = "$WINDOWS NT$"
Class       = Net
Provider    = %V_INTEL%
CatalogFile = e1d68x64.cat
DriverVer   = 03/15/2024,12.19.2.60

[Manufacturer]
%V_INTEL% = Intel, NTamd64.10.0, NTamd64.10.0.1

[Intel]
%E1D_DESC% = E1D.ndi, PCI\VEN_8086&DEV_15B7

[Intel.NTamd64.10.0]
%E1D_DESC% = E1D.ndi, PCI\VEN_8086&DEV_15B7
%E1D2_DESC% = E1D.ndi, PCI\VEN_8086&DEV_15B8, PCI\VEN_8086&DEV_15D6

[Intel.NTamd64.10.0.1]
%E1D_DESC% = E1D.ndi, PCI\VEN_8086&DEV_15B7

[E1D.ndi]
AddReg = E1D.reg

[Strings]
V_INTEL   = "Intel"
E1D_DESC  = "Intel(R) Ethernet Connection I219-V"
E1D2_DESC = "Intel(R) Ethernet Connection I219-LM"
`

func TestParseExtractsMetadataAndHWIDs(t *testing.T) {
	d, err := Parse([]byte(sampleINF))
	if err != nil {
		t.Fatal(err)
	}
	if d.Provider != "Intel" || d.ClassName != "Net" {
		t.Fatalf("provider/class = %q/%q", d.Provider, d.ClassName)
	}
	if d.Version != "12.19.2.60" || d.ReleaseDate != "03/15/2024" {
		t.Fatalf("version/date = %q/%q", d.Version, d.ReleaseDate)
	}
	if d.CatalogFile != "e1d68x64.cat" {
		t.Fatalf("catalog = %q", d.CatalogFile)
	}
	want := []string{
		`PCI\VEN_8086&DEV_15B7`,
		`PCI\VEN_8086&DEV_15B8`,
		`PCI\VEN_8086&DEV_15D6`,
	}
	got := map[string]bool{}
	for _, id := range d.HWIDs {
		got[id] = true
	}
	for _, id := range want {
		if !got[id] {
			t.Fatalf("missing hwid %s in %v", id, d.HWIDs)
		}
	}
	if len(d.HWIDs) != len(want) {
		t.Fatalf("hwids = %v", d.HWIDs)
	}
}

func TestParseUTF16LEWithBOM(t *testing.T) {
	u16 := utf16.Encode([]rune(sampleINF))
	buf := bytes.Buffer{}
	buf.Write([]byte{0xFF, 0xFE})
	for _, u := range u16 {
		buf.WriteByte(byte(u))
		buf.WriteByte(byte(u >> 8))
	}
	d, err := Parse(buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if d.Provider != "Intel" || len(d.HWIDs) != 3 {
		t.Fatalf("parsed = %#v", d)
	}
}

func TestParseGBKEncoded(t *testing.T) {
	gbkINF := `[Version]
Class = Net
Provider = %V%
DriverVer = 01/01/2020,1.0.0.0

[Manufacturer]
%V% = Models, NTamd64

[Models.NTamd64]
%DESC% = Install, PCI\VEN_10EC&DEV_8168

[Strings]
V = "瑞昱半导体"
DESC = "Realtek 网卡驱动"
`
	encoded, err := simplifiedchinese.GBK.NewEncoder().Bytes([]byte(gbkINF))
	if err != nil {
		t.Fatal(err)
	}
	d, err := Parse(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if d.Provider != "瑞昱半导体" {
		t.Fatalf("provider = %q", d.Provider)
	}
	if !reflect.DeepEqual(d.HWIDs, []string{`PCI\VEN_10EC&DEV_8168`}) {
		t.Fatalf("hwids = %v", d.HWIDs)
	}
}

func TestParseRejectsNonINF(t *testing.T) {
	if _, err := Parse([]byte("this is not an inf file at all")); err == nil {
		t.Fatal("expected error")
	}
}

func TestParseHandlesCommentsAndUndecoratedModels(t *testing.T) {
	inf := `[Version] ; header comment
Class = Display
Provider = "Vendor; Inc" ; provider has quoted semicolon
DriverVer = 06/01/2018,26.21.14.4141

[Manufacturer]
"Vendor; Inc" = VendorModels

[VendorModels]
Device One = Section064, PCI\VEN_10DE&DEV_1C82
Device Two = Section064, PCI\VEN_10DE&DEV_1C82 ; duplicate should dedupe
`
	d, err := Parse([]byte(inf))
	if err != nil {
		t.Fatal(err)
	}
	if d.ClassName != "Display" || d.Provider != "Vendor; Inc" {
		t.Fatalf("parsed = %#v", d)
	}
	if !reflect.DeepEqual(d.HWIDs, []string{`PCI\VEN_10DE&DEV_1C82`}) {
		t.Fatalf("hwids = %v", d.HWIDs)
	}
}

// HWID 列表会落库并经 API 返回，同一 zip 解析两次必须得到相同顺序。
func TestHardwareIDsAreOrderStable(t *testing.T) {
	// 多个型号节：提取按节名 map 遍历时，键多于一个顺序就会每次不同。
	inf := `[Version]
Class=Net
[Manufacturer]
%Intel%=IntelSection,NTamd64
%Realtek%=RealtekSection,NTamd64
%Broadcom%=BroadcomSection,NTamd64
[IntelSection.NTamd64]
%D1%=Install, PCI\VEN_8086&DEV_1000
%D2%=Install, PCI\VEN_8086&DEV_2000
[RealtekSection.NTamd64]
%D3%=Install, PCI\VEN_10EC&DEV_8168
%D4%=Install, PCI\VEN_10EC&DEV_8169
[BroadcomSection.NTamd64]
%D5%=Install, PCI\VEN_14E4&DEV_1600
`
	d, err := Parse([]byte(inf))
	if err != nil {
		t.Fatal(err)
	}
	if len(d.HWIDs) != 5 {
		t.Fatalf("expected the five ids, got %#v", d.HWIDs)
	}
	for i := 0; i < 20; i++ {
		again, err := Parse([]byte(inf))
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(again.HWIDs, d.HWIDs) {
			t.Fatalf("parse %d gave %#v, first gave %#v", i, again.HWIDs, d.HWIDs)
		}
	}
}
