package platform

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
)

// 服务器页从主机读这四项信息，解析出错不会报错，只会显示看似合理的错误值：
// os-release 引号写法不同、uptime 是浮点、网卡列表第一项是回环。

func writeFixture(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestOSPrettyNameReadsTheDistroLine(t *testing.T) {
	for _, tc := range []struct{ name, content, want string }{
		{
			name: "带引号",
			content: "NAME=\"Ubuntu\"\nVERSION=\"22.04.4 LTS\"\n" +
				"PRETTY_NAME=\"Ubuntu 22.04.4 LTS\"\nID=ubuntu\n",
			want: "Ubuntu 22.04.4 LTS",
		},
		{
			name:    "不带引号",
			content: "ID=openeuler\nPRETTY_NAME=openEuler 22.03 LTS\n",
			want:    "openEuler 22.03 LTS",
		},
		{
			// 名字相近但不是所要的行不能取。
			name:    "只有别的字段",
			content: "NAME=\"Ubuntu\"\nID=ubuntu\n",
			want:    "",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			old := osReleasePath
			osReleasePath = writeFixture(t, "os-release", tc.content)
			defer func() { osReleasePath = old }()
			if got := osPrettyName(); got != tc.want {
				t.Fatalf("= %q, want %q", got, tc.want)
			}
		})
	}
}

// 没有该文件的主机（容器、非 Linux 开发机）不报信息，但不让整个请求失败。
func TestOSPrettyNameIsEmptyWhenThereIsNoFile(t *testing.T) {
	old := osReleasePath
	osReleasePath = filepath.Join(t.TempDir(), "absent")
	defer func() { osReleasePath = old }()
	if got := osPrettyName(); got != "" {
		t.Fatalf("= %q, want empty", got)
	}
}

func TestHostUptimeTakesTheWholeSecondsOfTheFirstField(t *testing.T) {
	for _, tc := range []struct {
		name, content string
		want          int64
	}{
		{"正常两列", "123456.78 987654.32\n", 123456},
		{"只有一列", "42.0", 42},
		{"读不出来时为 0", "not a number\n", 0},
		{"空文件为 0", "", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			old := procUptimePath
			procUptimePath = writeFixture(t, "uptime", tc.content)
			defer func() { procUptimePath = old }()
			if got := hostUptimeSec(); got != tc.want {
				t.Fatalf("= %d, want %d", got, tc.want)
			}
		})
	}
}

func TestPrimaryIPv4SkipsLoopbackAndIPv6(t *testing.T) {
	old := interfaceAddrs
	defer func() { interfaceAddrs = old }()

	cidr := func(s string) net.Addr {
		ip, ipnet, err := net.ParseCIDR(s)
		if err != nil {
			t.Fatal(err)
		}
		ipnet.IP = ip
		return ipnet
	}
	interfaceAddrs = func() ([]net.Addr, error) {
		return []net.Addr{
			cidr("127.0.0.1/8"),       // 多数主机上回环排第一
			cidr("fe80::1/64"),        // 其次通常是链路本地 IPv6
			cidr("192.168.124.56/24"), // 操作者认得的那个地址
			cidr("10.0.0.1/8"),
		}, nil
	}
	if got := primaryIPv4(); got != "192.168.124.56" {
		t.Fatalf("= %q, want the first routable IPv4", got)
	}
}

func TestPrimaryIPv4IsEmptyWhenThereIsNothingToReport(t *testing.T) {
	old := interfaceAddrs
	defer func() { interfaceAddrs = old }()
	interfaceAddrs = func() ([]net.Addr, error) { return nil, net.ErrClosed }
	if got := primaryIPv4(); got != "" {
		t.Fatalf("= %q, want empty", got)
	}
}

// Info 汇总四项信息加版本号；缺任何一项仍要应答，出问题时操作者打开的正是这个页面。
func TestServerInfoAnswersEvenWhenTheHostTellsItNothing(t *testing.T) {
	oldOS, oldUp, oldAddrs := osReleasePath, procUptimePath, interfaceAddrs
	defer func() { osReleasePath, procUptimePath, interfaceAddrs = oldOS, oldUp, oldAddrs }()
	osReleasePath = filepath.Join(t.TempDir(), "absent")
	procUptimePath = filepath.Join(t.TempDir(), "absent")
	interfaceAddrs = func() ([]net.Addr, error) { return nil, net.ErrClosed }

	info, err := (ServiceService{Version: "v1.2.3"}).Info(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if info.Version != "v1.2.3" || info.Role != "all" {
		t.Fatalf("info = %#v", info)
	}
	if info.Hostname == "" {
		t.Fatal("hostname should still be reported")
	}
}
