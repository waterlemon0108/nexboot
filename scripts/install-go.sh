#!/usr/bin/env bash
set -euo pipefail

APP=ndiskless
PREFIX=/opt/ndiskless
DATA_DIR=/var/lib/ndiskless
ENV_DIR=/etc/ndiskless
ENV_FILE=$ENV_DIR/ndiskless.env
SERVICE_FILE=/etc/systemd/system/ndiskless.service
DNSMASQ_CONF=/etc/dnsmasq.d/ndiskless.conf
DNSMASQ_BASE_CONF=/etc/dnsmasq.d/00-ndiskless-base.conf
IMPORT_DIR=$DATA_DIR/imports

INSTALL_DEPS=0
# --provisioned：.deb 已放好二进制和 unit，ndiskless-configure 只做现场配置（env、池、HA、启动）。
# 默认 0 为源码/tarball 安装，全部自己铺。
PROVISIONED=0
BINARY=
POOL=tank
ADDR=:8080
BOOT_URL=
BOOTSTRAP_USER=admin
BOOTSTRAP_PASSWORD=
CREATE_POOL=
POOL_DISKS=
# HA 选项：--peer/--vip/--cluster-token 让本机加入主备集群；不给则为单机。
PEER_IP=
VIP=
CLUSTER_TOKEN=
NODE_ADDR=
JWT_SECRET=
CHAP_SECRET=
HA_ROLE=active
DEFER_KEEPALIVED=0

usage() {
  cat <<'EOF'
Usage:
  install-go.sh --binary ./ndiskless --bootstrap-password <password> [options]

Options:
  --install-deps                 Install Ubuntu/Debian runtime dependencies.
  --binary PATH                  ndiskless binary to install.
  --pool NAME                    ZFS pool used by ndiskless. Default: tank.
  --addr ADDR                    HTTP listen address. Default: :8080.
  --boot-url URL                 iPXE HTTP boot URL. Default: auto-detect from host IPv4 and --addr.
  --import-dir DIR               Server-side ZFS send file import directory. Default: /var/lib/ndiskless/imports.
  --bootstrap-user USER          Initial admin user. Default: admin.
  --bootstrap-password PASSWORD  Initial admin password. Required.
  --create-pool NAME             Create this ZFS pool if missing.
  --pool-disks CSV               Comma-separated disks for --create-pool.
  --chap-secret SECRET           iSCSI CHAP secret. Locks each client's disk
                                 behind a per-MAC login so no one else on the
                                 segment can attach it. Omit on a single node
                                 to auto-generate one; on a cluster it MUST be
                                 the same on every node (like --cluster-token),
                                 and omitting it there stays in demo mode.
  --defer-keepalived             Write keepalived's config but leave the daemon
                                 stopped, marking the join pending. The service
                                 starts it once the writer lists this node as a
                                 VRRP peer — starting earlier, before the master
                                 sends adverts here, elects a second master.
                                 Used by the UI join; not for hand installs.
  -h, --help                     Show this help.

Cluster options (a node is single-node unless --peer is given):
  --peer IP[,IP...]              Every OTHER node's real address (comma-separated),
                                 never the VIP. Enables HA and seeds keepalived's
                                 unicast peer list; the service keeps that list
                                 equal to the cluster roster afterwards, so nodes
                                 added later need no re-run here.
  --vip IP                       Virtual IP the clients and the API use. Giving
                                 it is what makes this node a cluster; --peer may
                                 be empty on the first node (no peers yet).
  --cluster-token TOKEN          Shared secret for the cluster channel; identical
                                 on every node. Omit it and one is generated (the
                                 first node) or the existing one kept (a re-run);
                                 pass it only to copy another node's value.
  --ha-role active|standby       Role this node starts in. Default: active.
                                 The FIRST node keeps active; every node that
                                 joins later must start as standby.
  --node-addr IP                 This node's OWN address (never the VIP) — what
                                 other nodes and clients placed on it dial.
                                 Required on every node once the cluster has
                                 more than one node.
  --jwt-secret HEX               Session signing key. Must be IDENTICAL on every
                                 node, otherwise a failover signs every operator
                                 out (the token one node minted does not verify
                                 on the next). Generated once on a fresh install
                                 and preserved across re-runs; pass it only to
                                 copy the first node's value onto the others.

Examples:
  # single node
  bash install-go.sh --install-deps --binary ./ndiskless --create-pool tank --pool-disks '/dev/sdb' --bootstrap-password 'secret'
  # second node of an HA pair (run on the new node first, then re-run on the old one)
  bash install-go.sh --install-deps --binary ./ndiskless --create-pool tank --pool-disks '/dev/sdb' \
      --bootstrap-password 'secret' --peer 192.168.10.3 --vip 192.168.10.250 \
      --cluster-token 'cluster-secret' --ha-role standby --node-addr 192.168.10.4
EOF
}

log() { printf '[INFO] %s\n' "$*"; }
warn() { printf '[WARN] %s\n' "$*" >&2; }
die() { printf '[ERROR] %s\n' "$*" >&2; exit 1; }

need_cmd() {
  command -v "$1" >/dev/null 2>&1 || die "missing command: $1"
}

random_hex() {
  if command -v openssl >/dev/null 2>&1; then
    openssl rand -hex 32
  else
    od -An -N32 -tx1 /dev/urandom | tr -d ' \n'
  fi
}

trim() {
  local value=$1
  value=${value#"${value%%[![:space:]]*}"}
  value=${value%"${value##*[![:space:]]}"}
  printf '%s' "$value"
}

parse_args() {
  while [ "$#" -gt 0 ]; do
    case "$1" in
      --install-deps) INSTALL_DEPS=1 ;;
      # 由 .deb 包装命令 ndiskless-configure 传入：二进制和 unit 已就位，只做现场配置。
      --provisioned) PROVISIONED=1 ;;
      --binary) shift; BINARY=${1:-} ;;
      --pool) shift; POOL=${1:-} ;;
      --addr) shift; ADDR=${1:-} ;;
      --boot-url) shift; BOOT_URL=${1:-} ;;
      --import-dir) shift; IMPORT_DIR=${1:-} ;;
      --bootstrap-user) shift; BOOTSTRAP_USER=${1:-} ;;
      --bootstrap-password) shift; BOOTSTRAP_PASSWORD=${1:-} ;;
      --create-pool) shift; CREATE_POOL=${1:-} ;;
      --pool-disks) shift; POOL_DISKS=${1:-} ;;
      # 逗号分隔可给多个，所有节点都参与 VRRP；只给一个即双机。
      --peer) shift; PEER_IP=${1:-} ;;
      --vip) shift; VIP=${1:-} ;;
      --cluster-token) shift; CLUSTER_TOKEN=${1:-} ;;
      --ha-role) shift; HA_ROLE=${1:-} ;;
      --node-addr) shift; NODE_ADDR=${1:-} ;;
      --jwt-secret) shift; JWT_SECRET=${1:-} ;;
      --chap-secret) shift; CHAP_SECRET=${1:-} ;;
      --defer-keepalived) DEFER_KEEPALIVED=1 ;;
      -h|--help) usage; exit 0 ;;
      *) die "unknown option: $1" ;;
    esac
    shift
  done
}

# is_ipv4 只接受每段都在 0..255 的点分四段地址。
is_ipv4() {
  local ip=$1 part parts
  [[ "$ip" =~ ^[0-9]{1,3}\.[0-9]{1,3}\.[0-9]{1,3}\.[0-9]{1,3}$ ]] || return 1
  IFS=. read -r -a parts <<< "$ip"
  for part in "${parts[@]}"; do
    [ "$((10#$part))" -le 255 ] || return 1
  done
}

has_control_chars() {
  [[ "$1" =~ [[:cntrl:]] ]]
}

# validate_args 必须在写任何文件之前运行：错误的值（如带 /24 的 VIP）若到 keepalived 步骤才失败，
# 集群配置已经落盘，节点会以没有 keepalived 的备机身份重启，控制台也无法再纳管它。
validate_args() {
  local peer peers name_ok='^[A-Za-z][A-Za-z0-9_.:-]*$' token_ok='^[A-Za-z0-9._:+=@-]+$'
  if [ -n "$VIP" ]; then
    is_ipv4 "$VIP" || die "--vip 不是合法的 IPv4 地址：只填地址本身，不带掩码和端口，例如 192.168.10.250"
  fi
  if [ -n "$PEER_IP" ]; then
    has_control_chars "$PEER_IP" && die "--peer 里有换行等控制字符：多个节点用逗号分隔，每个只填地址本身"
    IFS=, read -r -a peers <<< "$PEER_IP"
    for peer in "${peers[@]}"; do
      peer=$(trim "$peer")
      [ -n "$peer" ] || continue
      is_ipv4 "$peer" || die "--peer 里的「${peer}」不是合法的 IPv4 地址：多个节点用逗号分隔，每个只填地址本身"
    done
  fi
  if [ -n "$NODE_ADDR" ]; then
    is_ipv4 "$NODE_ADDR" || die "--node-addr 不是合法的 IPv4 地址：填本机在客户机网段上的地址"
  fi
  if [ -n "$POOL" ]; then
    [[ "$POOL" =~ $name_ok ]] || die "--pool 的池名不可用：只能用字母、数字和 _ - . :，并以字母开头"
  fi
  if [ -n "$CREATE_POOL" ]; then
    [[ "$CREATE_POOL" =~ $name_ok ]] || die "--create-pool 的池名不可用：只能用字母、数字和 _ - . :，并以字母开头"
  fi
  if [ -n "$CLUSTER_TOKEN" ]; then
    [[ "$CLUSTER_TOKEN" =~ $token_ok ]] || die "--cluster-token 含有不允许的字符：只能用字母、数字和 . _ - : + = @"
  fi
  has_control_chars "$JWT_SECRET" && die "--jwt-secret 里有换行等控制字符，请重新设置"
  has_control_chars "$CHAP_SECRET" && die "--chap-secret 里有换行等控制字符，请重新设置"
  has_control_chars "$BOOTSTRAP_PASSWORD" && die "--bootstrap-password 里有换行等控制字符，请重新设置"
  case "$HA_ROLE" in
    active|standby) ;;
    *) die "--ha-role 只能是 active 或 standby" ;;
  esac
  return 0
}

check_root_systemd() {
  [ "$(id -u)" -eq 0 ] || die "run as root"
  need_cmd systemctl
  [ -d /run/systemd/system ] || die "systemd is required"
}

check_debian_family() {
  [ -f /etc/os-release ] || die "/etc/os-release not found"
  . /etc/os-release
  case "${ID:-}" in
    ubuntu|debian) ;;
    *) die "unsupported OS: ${PRETTY_NAME:-unknown}; only Ubuntu/Debian is supported" ;;
  esac
}

install_deps() {
  check_debian_family
  need_cmd apt-get
  export DEBIAN_FRONTEND=noninteractive

  log "installing runtime packages"
  apt-get -o DPkg::Lock::Timeout=600 update
  # dnsmasq-utils 提供 dhcp_release，用于终端删除、改 IP、换组后释放旧租约。
  # 缺了它 dnsmasq 会因旧租约拒发保留地址（"not using configured address X because it is leased to Y"），
  # 但不会让任何操作失败。
  apt-get -o DPkg::Lock::Timeout=600 install -y curl dnsmasq dnsmasq-utils qemu-utils targetcli-fb zfsutils-linux

  if ! modprobe zfs >/dev/null 2>&1; then
    log "zfs module missing; installing dkms headers"
    apt-get -o DPkg::Lock::Timeout=600 install -y "linux-headers-$(uname -r)" zfs-dkms
    modprobe zfs
  fi

  modprobe target_core_mod
  modprobe iscsi_target_mod
  # ndiskless 直接写 LIO configfs（启动路径不走 targetcli），没人会自动加载这些模块，需持久化到重启后。
  printf 'target_core_mod\niscsi_target_mod\n' > /etc/modules-load.d/ndiskless.conf

  systemctl enable zfs-import.target >/dev/null 2>&1 || true
  systemctl start zfs-import.target >/dev/null 2>&1 || true
  install_dnsmasq_base_config
  systemctl enable --now dnsmasq
}

install_dnsmasq_base_config() {
  install -d -m 0755 "$(dirname "$DNSMASQ_BASE_CONF")"
  cat > "$DNSMASQ_BASE_CONF" <<'EOF'
# managed by ndiskless install-go.sh
# Keep Ubuntu's systemd-resolved on port 53; ndiskless uses dnsmasq for DHCP/TFTP.
port=0
EOF
}

check_runtime_commands() {
  need_cmd zfs
  need_cmd zpool
  need_cmd qemu-img
  need_cmd targetcli
  need_cmd dnsmasq
  zfs version >/dev/null 2>&1 || die "zfs is not usable"
  qemu-img --version >/dev/null 2>&1 || die "qemu-img is not usable"
  targetcli ls >/dev/null 2>&1 || die "targetcli is not usable"
  dnsmasq --version >/dev/null 2>&1 || die "dnsmasq is not usable"
}

ensure_pool() {
  if [ -n "$CREATE_POOL" ]; then
    POOL=$CREATE_POOL
    if zpool list "$POOL" >/dev/null 2>&1; then
      log "pool exists: $POOL"
      return
    fi
    [ -n "$POOL_DISKS" ] || die "--pool-disks is required with --create-pool"

    local disks=()
    local disk
    IFS=',' read -r -a disks <<< "$POOL_DISKS"
    [ "${#disks[@]}" -gt 0 ] || die "--pool-disks is empty"
    for i in "${!disks[@]}"; do
      disk=$(trim "${disks[$i]}")
      [ -n "$disk" ] || die "empty disk in --pool-disks"
      [ -b "$disk" ] || die "not a block device: $disk"
      disks[$i]=$disk
    done

    log "creating pool $POOL"
    zpool create "$POOL" "${disks[@]}"
    return
  fi

  if ! zpool list "$POOL" >/dev/null 2>&1; then
    warn "ZFS pool not found: $POOL; create it in the UI or rerun with --create-pool and --pool-disks"
  fi
}

check_port() {
  local port=${ADDR##*:}
  [[ "$port" =~ ^[0-9]+$ ]] || return 0
  if systemctl is-active --quiet "$APP" 2>/dev/null; then
    return 0
  fi
  if command -v ss >/dev/null 2>&1 && ss -ltn | awk '{print $4}' | grep -Eq "[:.]$port$"; then
    die "TCP port already in use: $port"
  fi
}

detect_boot_url() {
  [ -z "$BOOT_URL" ] || return 0
  local port host
  port=${ADDR##*:}
  [[ "$port" =~ ^[0-9]+$ ]] || die "--boot-url is required when --addr has no TCP port"
  host=${ADDR%:*}
  host=${host#\[}
  host=${host%\]}
  if [ -z "$host" ] || [ "$host" = "0.0.0.0" ] || [ "$host" = "::" ]; then
    if command -v ip >/dev/null 2>&1; then
      host=$(ip -4 route get 1.1.1.1 2>/dev/null | awk '{for (i=1; i<=NF; i++) if ($i=="src") {print $(i+1); exit}}')
    fi
    if [ -z "$host" ]; then
      host=$(hostname -I 2>/dev/null | awk '{print $1}')
    fi
  fi
  [ -n "$host" ] || die "could not auto-detect boot URL host; pass --boot-url"
  BOOT_URL="http://$host:$port/boot?mac=\${net0/mac}"
}

# ensure_dirs 创建 write_env 和服务需要的状态目录。--provisioned 下没有 install_binary 建目录，
# 缺 /etc/ndiskless 会让 write_env 失败。幂等。
ensure_dirs() {
  install -d -m 0755 "$DATA_DIR" "$ENV_DIR" "$IMPORT_DIR"
}

# prepare_runtime 做 install_deps 中与 apt 无关的主机准备：加载并持久化 LIO 模块、启用池自动导入、
# 写 dnsmasq 基础配置（让 systemd-resolved 保留 :53）。每次安装都跑，保证不带 --install-deps 的 .deb 安装可用。
# 幂等；模块加载失败只告警，纯控制节点不需要提供 iSCSI。
prepare_runtime() {
  modprobe target_core_mod 2>/dev/null || warn "could not load target_core_mod; load it before serving iSCSI clients"
  modprobe iscsi_target_mod 2>/dev/null || true
  printf 'target_core_mod\niscsi_target_mod\n' > /etc/modules-load.d/ndiskless.conf
  systemctl enable zfs-import.target >/dev/null 2>&1 || true
  systemctl start zfs-import.target >/dev/null 2>&1 || true
  install_dnsmasq_base_config
}

install_binary() {
  [ -n "$BINARY" ] || die "--binary is required"
  [ -f "$BINARY" ] || die "binary not found: $BINARY"
  [ -n "$IMPORT_DIR" ] || die "--import-dir is required"
  [[ "$IMPORT_DIR" = /* ]] || die "--import-dir must be absolute"
  install -d -m 0755 "$PREFIX" "$DATA_DIR" "$ENV_DIR" "$IMPORT_DIR"
  if [ -f "$PREFIX/$APP" ]; then
    cp -a "$PREFIX/$APP" "$PREFIX/$APP.$(date +%Y%m%d%H%M%S).bak"
  fi
  install -m 0755 "$BINARY" "$PREFIX/$APP"
}

# 本脚本管理的 env 键；文件里其余键归运维所有，重跑时保留。
MANAGED_ENV_KEYS='^NDISKLESS_(ADDR|DB_DSN|POOL|IMPORT_DIR|DNSMASQ_CONF|BOOT_URL|BOOTSTRAP_USERNAME|BOOTSTRAP_PASSWORD|JWT_SECRET|CHAP_SECRET|ROLE|PEER_URL|CLUSTER_TOKEN|PORTAL_ADDR|NODE_ADDR)='

# resolve_chap_secret 决定本机的 iSCSI CHAP 密钥并输出（"" 为演示模式）。各节点密钥不一致时，
# 客户机切到另一台后无法登录自己的盘，比演示模式更糟，因此：
#   给了 --chap-secret        直接用（集群里由运维保证各节点一致）
#   env 里已有                保留（重跑、升级不得换密钥）
#   单机且未给                生成一个
#   集群（--peer）且未给      留空：各节点随机生成的值必然不同
resolve_chap_secret() {
  if [ -n "$CHAP_SECRET" ]; then
    printf '%s' "$CHAP_SECRET"
    return
  fi
  if [ -f "$ENV_FILE" ] && grep -q '^NDISKLESS_CHAP_SECRET=' "$ENV_FILE"; then
    sed -n 's/^NDISKLESS_CHAP_SECRET=//p' "$ENV_FILE" | head -1
    return
  fi
  if [ -z "$VIP" ]; then
    random_hex
    return
  fi
  printf ''
}

# 集群令牌：显式给了就用；否则沿用 env 里已有的（重跑、升级不得更换）；都没有才生成新的
# （建集群的第一台），加入的节点由界面/纳管带入主机的值。
# 必须在 write_env 之前确定，否则写进 env 的是空值，节点之间无法互认。
resolve_cluster_token() {
  [ -n "$VIP" ] || return 0
  if [ -z "$CLUSTER_TOKEN" ] && [ -f "$ENV_FILE" ] && grep -q "^NDISKLESS_CLUSTER_TOKEN=" "$ENV_FILE"; then
    CLUSTER_TOKEN=$(sed -n "s/^NDISKLESS_CLUSTER_TOKEN=//p" "$ENV_FILE" | head -1)
    [ -n "$CLUSTER_TOKEN" ] && log "keeping the existing cluster token"
  fi
  if [ -z "$CLUSTER_TOKEN" ]; then
    CLUSTER_TOKEN=$(random_hex)
    log "generated a cluster token for this new cluster"
  fi
}

write_env() {
  # 重跑安装（单机转集群、界面入盟、升级）不要求重新输入管理员口令：显式参数优先，否则沿用 env。
  local bootstrap_password
  bootstrap_password=$BOOTSTRAP_PASSWORD
  if [ -z "$bootstrap_password" ] && [ -f "$ENV_FILE" ] && grep -q '^NDISKLESS_BOOTSTRAP_PASSWORD=' "$ENV_FILE"; then
    bootstrap_password=$(sed -n 's/^NDISKLESS_BOOTSTRAP_PASSWORD=//p' "$ENV_FILE" | head -1)
    log "keeping the existing admin password"
  fi
  [ -n "$bootstrap_password" ] || die "--bootstrap-password is required"
  local jwt_secret preserved peer_url
  # 重跑不得更换签名密钥：换了会让所有已登录的操作员掉线，且集群内一台签发的令牌要能在另一台验证。
  # 优先级：--jwt-secret > env 已有值 > 随机生成。
  if [ -n "$JWT_SECRET" ]; then
    jwt_secret=$JWT_SECRET
  elif [ -f "$ENV_FILE" ] && grep -q '^NDISKLESS_JWT_SECRET=' "$ENV_FILE"; then
    jwt_secret=$(sed -n 's/^NDISKLESS_JWT_SECRET=//p' "$ENV_FILE" | head -1)
    log "keeping the existing session signing key"
  else
    jwt_secret=$(random_hex)
  fi
  local chap_secret
  chap_secret=$(resolve_chap_secret)
  # 保留运维手工调整的键（复制速率、驱动目录等）。
  preserved=
  if [ -f "$ENV_FILE" ]; then
    preserved=$(grep -vE "$MANAGED_ENV_KEYS" "$ENV_FILE" | grep -vE '^\s*(#|$)' || true)
  fi
  umask 077
  cat > "$ENV_FILE" <<EOF
NDISKLESS_ADDR=$ADDR
NDISKLESS_DB_DSN=file:$DATA_DIR/ndiskless.db
NDISKLESS_POOL=$POOL
NDISKLESS_IMPORT_DIR=$IMPORT_DIR
NDISKLESS_DNSMASQ_CONF=$DNSMASQ_CONF
NDISKLESS_BOOT_URL=$BOOT_URL
NDISKLESS_BOOTSTRAP_USERNAME=$BOOTSTRAP_USER
NDISKLESS_BOOTSTRAP_PASSWORD=$bootstrap_password
NDISKLESS_JWT_SECRET=$jwt_secret
NDISKLESS_CHAP_SECRET=$chap_secret
EOF
  if [ -n "$VIP" ]; then
    # 没有对端时集合点就是 VIP 本身，本机即写入者；这也让产品知道本机已在集群中，不会重复受理创建集群。
    peer_url="http://${VIP}:${ADDR##*:}"
    [ -z "$PEER_IP" ] || peer_url="http://${PEER_IP%%,*}:${ADDR##*:}"
    cat >> "$ENV_FILE" <<EOF
NDISKLESS_ROLE=$HA_ROLE
NDISKLESS_PEER_URL=$peer_url
NDISKLESS_CLUSTER_TOKEN=$CLUSTER_TOKEN
NDISKLESS_PORTAL_ADDR=$VIP
EOF
    # NODE_ADDR 是本机自己的可达 IP（其它节点和客户机连接用，不是 VIP）；多节点集群必填。
    if [ -n "${NODE_ADDR:-}" ]; then
      echo "NDISKLESS_NODE_ADDR=$NODE_ADDR" >> "$ENV_FILE"
    fi
    printf "%s" "$CLUSTER_TOKEN" > /etc/ndiskless/cluster-token
    chmod 600 /etc/ndiskless/cluster-token
  fi
  if [ -n "$preserved" ]; then
    printf '%s\n' "$preserved" >> "$ENV_FILE"
  fi
  # 明确告知 CHAP 状态：它决定网段里任何人能否挂载客户机的盘。
  if [ -z "$chap_secret" ]; then
    warn "iSCSI CHAP off (demo mode): any host on the client network that guesses a target IQN can attach a client's disk. Pass --chap-secret (the SAME value on every node) to turn on per-client auth."
  elif [ -n "$CHAP_SECRET" ]; then
    log "iSCSI CHAP on (secret from --chap-secret)."
  elif [ -z "$VIP" ]; then
    log "iSCSI CHAP on (generated a secret for this single node). Nodes added from the console (服务器管理 → 添加节点) receive it automatically; only a command-line join needs --chap-secret with this node's NDISKLESS_CHAP_SECRET from $ENV_FILE."
  else
    log "iSCSI CHAP on (kept the existing secret)."
  fi
}

# 启动时角色标记优先于 env 默认值，曾被降级的机器会一直以备机起来，即使已持有 VIP。
# 显式 --ha-role 是运维的决定，写入标记；epoch 沿用不重置，它是防止陈旧主机胜出的栅栏。
seed_role_marker() {
  [ -n "$VIP" ] || return 0
  local file="$DATA_DIR/ha-role" epoch=0
  if [ -f "$file" ]; then
    # 用 [0-9][0-9]* 而非 [0-9]\+：BSD sed 不认 \+，匹配不到会把 epoch 重置为 0。
    epoch=$(sed -n 's/.*"epoch"[[:space:]]*:[[:space:]]*\([0-9][0-9]*\).*/\1/p' "$file" | head -1)
    [ -n "$epoch" ] || epoch=0
  fi
  printf '{"role":"%s","epoch":%s}\n' "$HA_ROLE" "$epoch" > "$file"
  log "role marker: $HA_ROLE (epoch $epoch)"
}

# nd_deploy_dir 定位 deploy/ 资产：源码/tarball 安装在 scripts/ 旁边，.deb 安装在 /opt/ndiskless/deploy。
nd_deploy_dir() {
  local d
  d=$(cd "$(dirname "$0")/../deploy" 2>/dev/null && pwd) && { printf '%s' "$d"; return; }
  [ -d /opt/ndiskless/deploy ] && { printf '/opt/ndiskless/deploy'; return; }
  die "deploy/ assets not found (looked next to scripts/ and in /opt/ndiskless/deploy)"
}

# write_service 安装 deploy/ 里唯一的 unit，不另写内联副本，避免两份不一致
# （例如缺了 zfs-import.target/zfs.target 的启动顺序）。
write_service() {
  install -m 0644 "$(nd_deploy_dir)/ndiskless.service" "$SERVICE_FILE"
}

start_service() {
  systemctl daemon-reload
  systemctl enable "$APP"
  if systemctl is-active --quiet "$APP"; then
    systemctl restart "$APP"
  else
    systemctl start "$APP"
  fi
}

health_check() {
  local port=${ADDR##*:}
  [[ "$port" =~ ^[0-9]+$ ]] || { warn "skip health check for non-port addr: $ADDR"; return 0; }
  if ! command -v curl >/dev/null 2>&1; then
    warn "curl missing; skipped HTTP health check"
    return 0
  fi

  # /healthz 回答的是「该不该持有 VIP」，新装的备机等情况会如实返回 503。
  # 装机只需确认服务在监听：能应答就算装好，非 200 时打印原因供人判断。
  local code body
  for _ in $(seq 1 20); do
    # || true 不能省：set -e 下服务未起时 curl 返回 7，会让脚本在此静默中止。
    code=$(curl -s -o /dev/null -w '%{http_code}' -m 3 "http://127.0.0.1:$port/healthz" 2>/dev/null || true)
    if [ "$code" = "200" ]; then
      log "health check ok: http://127.0.0.1:$port/healthz"
      return 0
    fi
    if [ -n "$code" ] && [ "$code" != "000" ]; then
      body=$(curl -s -m 3 "http://127.0.0.1:$port/healthz" 2>/dev/null || true)
      log "service is up (healthz $code): $body"
      return 0
    fi
    sleep 1
  done
  systemctl status "$APP" --no-pager -l || true
  die "health check failed"
}

# 建新集群（给了 --vip 没给 --peer）前确认 VIP 未被占用。否则两个集群共用一个 VRRP 组和虚 IP，
# 令牌不同互拒通告，各自成为 MASTER，同一地址挂在两台机器上。
# 此时应保持本机出厂态，到已有集群的控制台里添加它。
refuse_if_vip_taken() {
  [ -n "$VIP" ] || return 0
  [ -z "$PEER_IP" ] || return 0   # 明确指了对端 = 加入已知集群，不是新建
  command -v curl >/dev/null 2>&1 || return 0
  local port=${ADDR##*:} body
  body=$(curl -s --max-time 3 "http://$VIP:$port/internal/cluster/identity" 2>/dev/null) || return 0
  [ -n "$body" ] || return 0
  die "虚 IP $VIP 已经被一个集群占用（它回应了 $VIP:$port）。不要在这里新建集群——
     请让本机保持出厂态（不要传 --vip），然后到那个集群的控制台「添加节点」里把它加进来。
     如果确认那个集群已经作废，先把它停掉或换一个虚 IP。"
}

setup_keepalived() {
  # 集群模式由 --vip 触发而非 --peer：第一台没有对端，单播列表为空，直接成为 MASTER；
  # 后续节点加入时由服务按花名册补齐对端。
  [ -n "$VIP" ] || return 0
# standby_rank 给出本机地址在集群地址中的序号（从 0 起），让备机 VRRP 优先级互不相同：
# 最小地址为 100，其余递增，仍远低于主机的 150。
standby_rank() {
  local self=$1 peers=${2:-} rank
  rank=$(printf '%s\n%s\n' "$self" "$(printf '%s' "$peers" | tr ',' '\n')" \
         | sed 's/^[[:space:]]*//; s/[[:space:]]*$//' \
         | awk 'NF' \
         | sort -u -t. -k1,1n -k2,2n -k3,3n -k4,4n \
         | grep -n -x -F "$self" | head -1 | cut -d: -f1)
  echo $(( ${rank:-1} - 1 ))
}

  apt-get -o DPkg::Lock::Timeout=600 install -y keepalived >/dev/null
  local iface self_ip port deploy_dir
  port=${ADDR##*:}
  deploy_dir=$(cd "$(dirname "$0")/../deploy" 2>/dev/null && pwd) || die "deploy/ directory not found next to scripts/"
  # 面向客户机的网卡是 VIP 所在 /24 的那块。
  iface=$(ip -o -4 addr show | awk -v vip="$VIP" '{ split($4, a, "/"); split(a[1], ip, "."); split(vip, v, "."); if (ip[1]==v[1] && ip[2]==v[2] && ip[3]==v[3]) { print $2; exit } }')
  [ -n "$iface" ] || die "no interface on the VIP's /24 ($VIP); bring the client NIC up first"
  self_ip=$(ip -o -4 addr show dev "$iface" | awk '{split($4,a,"/"); print a[1]; exit}')

  # 持有目录的节点必须赢得选举，否则新装节点的空目录会被复制覆盖真实镜像。
  # 备机优先级按地址排序而不是统一 100：三台时两台备机同优先级会同时宣告 MASTER，
  # 以相同 epoch 双双成为写入者。
  local priority=100
  if [ "$HA_ROLE" = "active" ]; then
    priority=150
  else
    priority=$((100 + $(standby_rank "$self_ip" "$PEER_IP")))
  fi
  # 每个对端一行：所有节点都参与 VRRP，否则第三台起的节点永远无法接管。
  local peer_block
  peer_block=$(printf '%s' "$PEER_IP" | tr ',' '\n' | sed 's/^[[:space:]]*//; s/[[:space:]]*$//' \
               | awk 'NF { lines[++n] = "        " $0 } END { for (i = 1; i <= n; i++) printf "%s%s", lines[i], (i < n ? "\\n" : "") }')
  sed -e "s/@IFACE@/$iface/" -e "s/@VIP@/$VIP/" -e "s/@SELF_IP@/$self_ip/" \
      -e "s/@AUTH_PASS@/$(printf %.8s "$CLUSTER_TOKEN")/" \
      -e "s/@PRIORITY@/$priority/" \
      "$deploy_dir/keepalived.conf.tmpl" \
    | sed -e "s|^ *@PEER_IP@ *$|$peer_block|" > /etc/keepalived/keepalived.conf
  sed "s/@API_PORT@/$port/" "$deploy_dir/ndiskless-check.sh" > /etc/keepalived/ndiskless-check.sh
  sed "s/@API_PORT@/$port/" "$deploy_dir/ndiskless-notify.sh" > /etc/keepalived/ndiskless-notify.sh
  chmod 755 /etc/keepalived/ndiskless-check.sh /etc/keepalived/ndiskless-notify.sh

  # 两个权威 DHCP 会互相 NAK 对方的客户机，dnsmasq 不能自行启动：由主机配置同步启动，备机启动时停掉。
  systemctl disable dnsmasq >/dev/null 2>&1 || true
  if [ "$DEFER_KEEPALIVED" -eq 1 ]; then
    # 界面入盟：写入者的对端列表里还没有本机，收不到通告会自选为主。先不开 VRRP 并标记待加入，
    # 等写入者的对端列表包含本机后由 ndiskless 启动 keepalived。
    systemctl disable --now keepalived >/dev/null 2>&1 || true
    touch "$ENV_DIR/keepalived.pending"
    log "keepalived configured but not started (join pending): the service starts it once the writer lists this node"
  else
    systemctl enable --now keepalived
  fi
  log "keepalived configured: vip=$VIP iface=$iface peers=$PEER_IP role=$HA_ROLE priority=$priority"
}

main() {
  parse_args "$@"
  validate_args
  check_root_systemd
  [ "$INSTALL_DEPS" -eq 0 ] || install_deps
  check_runtime_commands
  ensure_pool
  check_port
  detect_boot_url
  # TFTP 启动文件已内嵌在二进制里，每次启动写到 /srv/tftp，这里无需准备。
  # --provisioned 下跳过二进制和 unit 的安装。
  if [ "$PROVISIONED" -eq 0 ]; then
    install_binary
  fi
  # VIP 占用检查必须在写任何东西之前：否则 env 里留下半截集群配置，机器既不属于任何集群，
  # 也不再是出厂态，无法被纳管。
  refuse_if_vip_taken
  ensure_dirs
  prepare_runtime
  resolve_cluster_token
  write_env
  seed_role_marker
  if [ "$PROVISIONED" -eq 0 ]; then
    write_service
  fi
  setup_keepalived
  start_service
  health_check

  log "installed $APP"
  local port=${ADDR##*:}
  if [[ "$port" =~ ^[0-9]+$ ]]; then
    log "open http://<server-ip>:$port"
  fi
  # 这里不打离线包，确有离线安装需求时再加。
}

main "$@"
