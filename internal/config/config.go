package config

import (
	"flag"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

const DefaultImportDir = "/var/lib/ndiskless/imports"
const DefaultDriverDir = "/var/lib/ndiskless/drivers"

type Config struct {
	Addr          string
	DBDSN         string
	Pool          string
	ImportDir     string
	DriverDir     string
	DnsmasqConf   string
	BootURL       string
	Workers       int
	MigrateOnly   bool
	JWTSecret     string
	BootstrapUser string
	BootstrapPass string
	// NodeID 是本机在库里的名字，servers、pools 和克隆都挂在它下面。取自 machine-id，天然稳定，
	// 改地址或把 portal 移到 VIP 都不会让目录换键。
	NodeID string
	// PortalAddr 是客户机访问本节点磁盘和启动端点的地址，即写进 sanhook 的地址。默认取启动 URL 的主机名；HA 下为 VIP。
	PortalAddr string
	// Role 是本进程的初始角色：默认 "all"；HA 下由角色标记改为 "active" 或 "standby"。
	Role string
	// PeerURL 是集群汇合点：VIP 的 API 基址（普通双机时为对端的）。为空表示单机。
	PeerURL string
	// NodeAddr 是本节点自己的可达 IP，供其他节点和被放置的客户机回拨，从不是 VIP。为空时回落到 PortalAddr（单机时二者相同）。
	NodeAddr string
	// ClusterToken 用于节点间复制通道等集群接口的认证。
	ClusterToken string
	// ReplicationInterval 同时决定主机打标记和备机拉取的节奏。
	ReplicationInterval time.Duration
	// ReplicationRateMBPerSec 限制批量复制流（新节点首次全量同步、promote 后重建），免得挤占共享上行链路上
	// 客户机的 iSCSI 流量。0 表示不限速。
	ReplicationRateMBPerSec int
}

func Load(args []string) Config {
	cfg := Config{
		Addr:          env("NDISKLESS_ADDR", ":8080"),
		DBDSN:         env("NDISKLESS_DB_DSN", "file:ndiskless.db"),
		Pool:          env("NDISKLESS_POOL", "tank"),
		ImportDir:     env("NDISKLESS_IMPORT_DIR", DefaultImportDir),
		DriverDir:     env("NDISKLESS_DRIVER_DIR", DefaultDriverDir),
		DnsmasqConf:   env("NDISKLESS_DNSMASQ_CONF", "/etc/dnsmasq.d/ndiskless.conf"),
		BootURL:       env("NDISKLESS_BOOT_URL", ""),
		Workers:       envInt("NDISKLESS_WORKERS", 4),
		JWTSecret:     env("NDISKLESS_JWT_SECRET", ""),
		BootstrapUser: env("NDISKLESS_BOOTSTRAP_USERNAME", "admin"),
		BootstrapPass: env("NDISKLESS_BOOTSTRAP_PASSWORD", ""),
		NodeID:        env("NDISKLESS_NODE_ID", ""),
		PortalAddr:    env("NDISKLESS_PORTAL_ADDR", ""),
		Role:          env("NDISKLESS_ROLE", "all"),
		PeerURL:       env("NDISKLESS_PEER_URL", ""),
		NodeAddr:      env("NDISKLESS_NODE_ADDR", ""),
		ClusterToken:  env("NDISKLESS_CLUSTER_TOKEN", ""),
	}
	cfg.ReplicationInterval = envDuration("NDISKLESS_REPLICATION_INTERVAL", time.Minute)
	cfg.ReplicationRateMBPerSec = envInt("NDISKLESS_REPLICATION_RATE_MBPS", 100)

	fs := flag.NewFlagSet("ndiskless", flag.ExitOnError)
	fs.StringVar(&cfg.Addr, "addr", cfg.Addr, "HTTP listen address")
	fs.StringVar(&cfg.DBDSN, "db-dsn", cfg.DBDSN, "database DSN")
	fs.StringVar(&cfg.Pool, "pool", cfg.Pool, "default ZFS pool name")
	fs.StringVar(&cfg.ImportDir, "import-dir", cfg.ImportDir, "server-side ZFS send import directory")
	fs.StringVar(&cfg.DriverDir, "driver-dir", cfg.DriverDir, "driver pack storage directory")
	fs.StringVar(&cfg.DnsmasqConf, "dnsmasq-conf", cfg.DnsmasqConf, "dnsmasq managed config path")
	fs.StringVar(&cfg.BootURL, "boot-url", cfg.BootURL, "iPXE HTTP chainload URL, e.g. http://<server>:8080/boot?mac=${net0/mac} (empty disables HTTP chainload; hardware-profile params platform/busid/manufacturer/product are appended automatically unless already present)")
	fs.IntVar(&cfg.Workers, "workers", cfg.Workers, "background worker count")
	fs.StringVar(&cfg.JWTSecret, "jwt-secret", cfg.JWTSecret, "JWT signing secret")
	fs.StringVar(&cfg.BootstrapUser, "bootstrap-user", cfg.BootstrapUser, "initial admin username")
	fs.StringVar(&cfg.BootstrapPass, "bootstrap-password", cfg.BootstrapPass, "initial admin password")
	fs.BoolVar(&cfg.MigrateOnly, "migrate-only", false, "run database migrations and exit")
	fs.StringVar(&cfg.NodeID, "node-id", cfg.NodeID, "stable identity of this node in the DB (default: derived from /etc/machine-id)")
	fs.StringVar(&cfg.PortalAddr, "portal-addr", cfg.PortalAddr, "address clients reach this node's disks at (default: the boot URL's host)")
	fs.StringVar(&cfg.Role, "role", cfg.Role, "process role: all")
	_ = fs.Parse(args)

	if cfg.PortalAddr == "" {
		cfg.PortalAddr = hostOfURL(cfg.BootURL)
	}
	if cfg.NodeID == "" {
		cfg.NodeID = machineNodeID()
	}
	return cfg
}

// hostOfURL 返回 URL 的主机名部分，解析失败时为 ""。
func hostOfURL(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return ""
	}
	return u.Hostname()
}

// machineNodeID 派生稳定的节点身份：/etc/machine-id 的前 12 个十六进制字符，没有时回落到主机名。
// 库里的 servers、pools、克隆都以它为键，稳定是关键。
func machineNodeID() string {
	if b, err := os.ReadFile("/etc/machine-id"); err == nil {
		if id := strings.TrimSpace(string(b)); len(id) >= 12 {
			return id[:12]
		} else if id != "" {
			return id
		}
	}
	if host, err := os.Hostname(); err == nil && host != "" {
		return host
	}
	return "local"
}

func envDuration(key string, fallback time.Duration) time.Duration {
	v, err := time.ParseDuration(os.Getenv(key))
	if err != nil || v <= 0 {
		return fallback
	}
	return v
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envInt(key string, fallback int) int {
	v, err := strconv.Atoi(os.Getenv(key))
	if err != nil || v <= 0 {
		return fallback
	}
	return v
}
