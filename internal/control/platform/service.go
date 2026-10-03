package platform

import (
	"bufio"
	"context"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/tianwei/diskless/internal/control/errs"
	"github.com/tianwei/diskless/internal/systemd"
)

var (
	ErrServiceNotFound = errs.NotFound("服务不存在")
	ErrServiceReadOnly = errs.Conflict("该服务为只读")
	ErrServiceAction   = errs.Invalid("服务操作无效")
)

// systemdClient 是这里用到的 systemd.Client 子集，便于测试替身。
type systemdClient interface {
	Show(ctx context.Context, unit string) (systemd.UnitStatus, error)
	Action(ctx context.Context, unit, action string) error
	Logs(ctx context.Context, unit string, lines int) (string, error)
}

// ManagedUnit 是控制台允许查看/控制的一个 systemd 单元。
type ManagedUnit struct {
	Key      string
	Unit     string
	Label    string
	Critical bool // 停掉会影响客户机开机，界面二次确认
	ReadOnly bool // 只看状态（如 ndiskless 自身），不允许启停
	// ActiveOnly 表示该单元只应在持有服务的节点上运行：第二个权威 dnsmasq 会 NAK
	// 主机的客户机，所以备机上 stopped 才是正确状态，不能报成故障。
	ActiveOnly bool
	// OneShot 表示单元开机做完事即退出，inactive 是常态，单元状态说明不了能力是否还在。
	OneShot bool
	// Probe 判断能力当下是否可用（如 LIO 的 configfs 树），oneshot 单元以它为准。
	Probe func() bool
	// Capability 是该单元以操作者的话说提供的能力（「客户机磁盘」而非「target.service」）。
	// 界面先展示能力，单元名、ActiveState、开机自启只放在详情里，否则会成为常驻误报来源。
	Capability string
}

// ServiceService 报告主机信息并管理固定白名单内的服务。
type ServiceService struct {
	Client  systemdClient
	Units   []ManagedUnit
	Version string
	// Role 返回本节点 HA 角色（"active"/"standby"）；为空或 nil 表示单机，全部服务都应运行。
	Role func() string
}

type ServiceItem struct {
	Key         string  `json:"key"`
	Label       string  `json:"label"`
	Unit        string  `json:"unit"`
	Critical    bool    `json:"critical"`
	ReadOnly    bool    `json:"read_only"`
	Installed   bool    `json:"installed"`
	Status      string  `json:"status"` // running / stopped / failed / activating / not-installed / unknown
	ActiveState string  `json:"active_state"`
	SubState    string  `json:"sub_state"`
	Enabled     bool    `json:"enabled"`
	MainPID     int     `json:"main_pid"`
	ActiveSince *string `json:"active_since"` // RFC3339，inactive/unknown 时为 null
	// Expected 是该服务此刻在本节点应处的状态（"running" / "stopped" / "ready"），OK 表示是否符合。
	// 只问「是否在运行」在备机（DHCP 必须停）和 oneshot 单元（本就退出）上都是错的。
	Expected string `json:"expected"`
	OK       bool   `json:"ok"`
	// Detail 解释看似异常其实正常的状态；需要大量解释的状态通常本就不该展示。
	Detail string `json:"detail"`
	// Capability 见 ManagedUnit.Capability；Provided 表示本节点是否提供该能力，
	// 备机不提供 DHCP 既不是故障也不是运行中。
	Capability string `json:"capability"`
	Provided   bool   `json:"provided"`
}

type ServiceListResult struct {
	Items []ServiceItem `json:"items"`
	// Role 是本节点的自报角色，随服务一起带回，汇总方不必依赖复制滞后的节点表。
	Role string `json:"role,omitempty"`
	// Host 是本节点自身的主机名、OS、运行时长和版本。控制台经 VIP 访问集群，
	// 不随这里带回就只能看到持有 VIP 的那台，滚动升级时看不出版本不一致。
	Host *HostFacts `json:"host,omitempty"`
}

// HostFacts 是只有本机自己才能报告的信息。
type HostFacts struct {
	Hostname  string `json:"hostname,omitempty"`
	OS        string `json:"os,omitempty"`
	UptimeSec int64  `json:"uptime_sec,omitempty"`
	Version   string `json:"version,omitempty"`
}

type ServerInfo struct {
	Hostname  string `json:"hostname"`
	IP        string `json:"ip"`
	OS        string `json:"os"`
	UptimeSec int64  `json:"uptime_sec"`
	Version   string `json:"version"`
	Role      string `json:"role"`
}

var allowedActions = map[string]bool{"start": true, "stop": true, "restart": true}

func (s ServiceService) unit(key string) (ManagedUnit, bool) {
	for _, u := range s.Units {
		if u.Key == key {
			return u, true
		}
	}
	return ManagedUnit{}, false
}

func (s ServiceService) ListServices(ctx context.Context) (ServiceListResult, error) {
	items := make([]ServiceItem, 0, len(s.Units))
	for _, u := range s.Units {
		item := ServiceItem{Key: u.Key, Label: u.Label, Unit: u.Unit, Critical: u.Critical, ReadOnly: u.ReadOnly}
		st, err := s.Client.Show(ctx, u.Unit)
		if err != nil {
			// systemctl 不可用（非 systemd 主机）时报 unknown，不让整个列表失败。
			item.Status = "unknown"
			items = append(items, item)
			continue
		}
		item.Installed = st.Installed()
		item.ActiveState = st.Active
		item.SubState = st.Sub
		item.Enabled = st.Enabled()
		item.MainPID = st.MainPID
		item.Status = deriveStatus(st)
		if !st.ActiveSince.IsZero() {
			iso := st.ActiveSince.Format(time.RFC3339)
			item.ActiveSince = &iso
		}
		item.Capability = u.Capability
		if item.Capability == "" {
			item.Capability = u.Label
		}
		item.Expected, item.OK, item.Detail = judge(u, item, s.role())
		item.Provided = providedHere(u, item, s.role())
		items = append(items, item)
	}
	res := ServiceListResult{Items: items, Role: s.role()}
	if info, err := s.Info(ctx); err == nil {
		res.Host = &HostFacts{Hostname: info.Hostname, OS: info.OS, UptimeSec: info.UptimeSec, Version: info.Version}
	}
	return res, nil
}

// providedHere 判断本节点是否提供该能力：只有写入者提供 DHCP，其余能力每个节点都自己提供。
func providedHere(u ManagedUnit, item ServiceItem, role string) bool {
	if u.ActiveOnly && role == "standby" {
		return false
	}
	return item.OK
}

func (s ServiceService) role() string {
	if s.Role == nil {
		return ""
	}
	return s.Role()
}

// judge 判断服务「此处是否应运行、实际是否如此」。只看 active 在两种常见情况下是错的：
// 备机不能跑 DHCP，oneshot 单元本就退出。
func judge(u ManagedUnit, item ServiceItem, role string) (expected string, ok bool, detail string) {
	switch {
	case u.OneShot:
		// 单元自身状态无意义，看能力是否在；没有探针时只作展示。
		if u.Probe == nil {
			return "ready", true, ""
		}
		if u.Probe() {
			return "ready", true, ""
		}
		return "ready", false, "内核态缺失：客户机无法连接磁盘"
	case u.ActiveOnly && role == "standby":
		if item.Status == "running" {
			return "stopped", false, "备机不应提供该服务，会与主机争抢客户机"
		}
		return "stopped", true, ""
	case item.Status == "not-installed":
		return "running", false, "未安装"
	default:
		return "running", item.Status == "running", ""
	}
}

func deriveStatus(st systemd.UnitStatus) string {
	if !st.Installed() {
		return "not-installed"
	}
	switch st.Active {
	case "active":
		return "running"
	case "failed":
		return "failed"
	case "activating", "deactivating", "reloading":
		return st.Active
	default:
		return "stopped"
	}
}

func (s ServiceService) ServiceAction(ctx context.Context, key, action string) error {
	u, ok := s.unit(key)
	if !ok {
		return ErrServiceNotFound
	}
	if u.ReadOnly {
		return ErrServiceReadOnly
	}
	if !allowedActions[action] {
		return ErrServiceAction
	}
	return s.Client.Action(ctx, u.Unit, action)
}

func (s ServiceService) ServiceLogs(ctx context.Context, key string, lines int) (string, error) {
	u, ok := s.unit(key)
	if !ok {
		return "", ErrServiceNotFound
	}
	return s.Client.Logs(ctx, u.Unit, lines)
}

func (s ServiceService) Info(ctx context.Context) (ServerInfo, error) {
	info := ServerInfo{Version: s.Version, Role: "all"}
	if h, err := os.Hostname(); err == nil {
		info.Hostname = h
	}
	info.OS = osPrettyName()
	info.UptimeSec = hostUptimeSec()
	info.IP = primaryIPv4()
	return info, nil
}

// 主机信息的来源路径，做成变量便于用固定样本测试解析逻辑。
var (
	osReleasePath  = "/etc/os-release"
	procUptimePath = "/proc/uptime"
	interfaceAddrs = net.InterfaceAddrs
)

func osPrettyName() string {
	f, err := os.Open(osReleasePath)
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "PRETTY_NAME=") {
			return strings.Trim(strings.TrimPrefix(line, "PRETTY_NAME="), "\"")
		}
	}
	return ""
}

func hostUptimeSec() int64 {
	b, err := os.ReadFile(procUptimePath)
	if err != nil {
		return 0
	}
	fields := strings.Fields(string(b))
	if len(fields) == 0 {
		return 0
	}
	f, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return 0
	}
	return int64(f)
}

func primaryIPv4() string {
	addrs, err := interfaceAddrs()
	if err != nil {
		return ""
	}
	for _, a := range addrs {
		if ipnet, ok := a.(*net.IPNet); ok && !ipnet.IP.IsLoopback() {
			if v4 := ipnet.IP.To4(); v4 != nil {
				return v4.String()
			}
		}
	}
	return ""
}
