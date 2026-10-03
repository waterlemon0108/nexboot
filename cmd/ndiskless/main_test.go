package main

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
)

func TestStorageServerFromBootURL(t *testing.T) {
	got := storageServerFromBootURL("http://192.168.211.4:8000/boot?mac=${net0/mac}")
	if got != "192.168.211.4" {
		t.Fatalf("server = %q", got)
	}
}

func TestBootURLWithHints(t *testing.T) {
	got := bootURLWithHints("http://192.168.211.4:8000/boot?mac=${net0/mac}")
	want := "http://192.168.211.4:8000/boot?mac=${net0/mac}&platform=${platform}&busid=${net0/busid}&manufacturer=${manufacturer:uristring}&product=${product:uristring}"
	if got != want {
		t.Fatalf("url = %q", got)
	}
	// 运维自定义的 URL 和空 URL 保持不动。
	custom := "http://x/boot?mac=${net0/mac}&platform=${platform}"
	if bootURLWithHints(custom) != custom {
		t.Fatalf("custom url modified: %q", bootURLWithHints(custom))
	}
	if bootURLWithHints("") != "" {
		t.Fatal("empty url modified")
	}
}

// 启动时 dnsmasq 同步失败不能退出进程：出错的配置就在库里，退出后没有界面和接口能去改它。
func TestStartupDHCPSyncFailureDoesNotStopTheServer(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	syncDHCPAtStartup(context.Background(), func(context.Context) error {
		return errors.New("bad dhcp-range at line 13")
	}, logger)
	// 走到这里就说明没有退出进程。
	if !strings.Contains(buf.String(), "bad dhcp-range at line 13") {
		t.Fatalf("失败没有被记下来：%s", buf.String())
	}
}

func TestLocalIPv4AddrsReadsSomething(t *testing.T) {
	addrs, err := localIPv4Addrs()
	if err != nil {
		t.Skipf("这台机器读不到网卡：%v", err)
	}
	// 回环地址至少得在，否则解析那一步是坏的。
	for _, addr := range addrs {
		if addr.IsLoopback() {
			return
		}
	}
	t.Fatalf("解析出的地址里没有回环：%v", addrs)
}

// 版本号要答得上「你装的什么版本」，deb 包版本也从这里来；否则只能逐台 sha256sum 比对。
func TestVersionLineNamesTheBuild(t *testing.T) {
	line := versionLine()
	if !strings.Contains(line, "ndiskless") {
		t.Fatalf("版本行没有名字：%q", line)
	}
	if !strings.Contains(line, version) {
		t.Fatalf("版本行没有版本号：%q（version=%q）", line, version)
	}
}

// --version 必须在拿进程锁之前处理：运维要在服务正运行的机器上查磁盘上那份二进制的版本。
func TestVersionRequestedRecognizesTheUsualSpellings(t *testing.T) {
	for _, arg := range []string{"--version", "-version", "version"} {
		if !versionRequested([]string{arg}) {
			t.Fatalf("%s 没被认出来", arg)
		}
	}
	if versionRequested([]string{"migrate-layout"}) || versionRequested(nil) {
		t.Fatalf("不该把别的参数当成版本查询")
	}
}
