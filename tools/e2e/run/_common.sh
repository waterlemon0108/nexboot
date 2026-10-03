# 被各脚本 source：读入 env 并做最基本的校验。
set -euo pipefail
HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
E2E=$(cd "$HERE/.." && pwd)
ENV_FILE=${ND_ENV:-$HERE/env}
if [ ! -f "$ENV_FILE" ]; then
  echo "找不到 $ENV_FILE —— 先 cp $HERE/env.example $HERE/env 并按现场改" >&2
  exit 2
fi
set -a; . "$ENV_FILE"; set +a
: "${ND_NODES:?env 里缺 ND_NODES}"
: "${ND_VIP:?env 里缺 ND_VIP}"
: "${ND_PASSWORD:?env 里缺 ND_PASSWORD}"
cd "$E2E"

# 集群令牌在「创建集群」时随机生成，env 里那份每次重建集群都会过期，
# 过期后 /internal/ 用例会集体报「cluster token required」。以本机产品配置为准；
# 在集群外跑读不到时才用 env。
if [ -r /etc/ndiskless/ndiskless.env ] || sudo -n true 2>/dev/null; then
  real_token=$(sudo -n sed -n 's/^NDISKLESS_CLUSTER_TOKEN=//p' /etc/ndiskless/ndiskless.env 2>/dev/null | tr -d '"' | head -1 || true)
  if [ -z "${real_token:-}" ] && [ -n "${ND_SSH_PASSWORD:-${ND_PASSWORD:-}}" ]; then
    real_token=$(echo "${ND_SSH_PASSWORD:-$ND_PASSWORD}" | sudo -S -p '' \
      sed -n 's/^NDISKLESS_CLUSTER_TOKEN=//p' /etc/ndiskless/ndiskless.env 2>/dev/null | tr -d '"' | head -1 || true)
  fi
  if [ -n "${real_token:-}" ] && [ "${real_token}" != "${ND_CLUSTER_TOKEN:-}" ]; then
    echo "集群令牌以本机产品配置为准（env 里那份已过期）" >&2
    export ND_CLUSTER_TOKEN="$real_token"
  fi
fi

# writer_and_other 打印「当前写入者 另一台备机」。三台都参选 VRRP，VIP 可能在任一台，
# 而 ha_matrix 的计划切换要求 A 是此刻的写入者，所以不能写死。
writer_and_other() {
  python3 -c "
import os, json, urllib.request
nodes = os.environ['ND_NODES'].split(',')
tok = os.environ.get('ND_CLUSTER_TOKEN', '')
active, standby = '', ''
for b in nodes:
    try:
        rq = urllib.request.Request(b + '/internal/ha/status', headers={'Authorization': 'Bearer ' + tok})
        st = json.load(urllib.request.urlopen(rq, timeout=4))
        if st.get('role') == 'active': active = b
        elif not standby: standby = b
    except Exception: pass
active = active or nodes[0]
standby = standby or next((n for n in nodes if n != active), nodes[-1])
print(active, standby)
"
}

wait_roster() {
  python3 -c "
import sys; sys.path.insert(0, '.')
from ndcluster import wait_roster_online, entry_base
n = len('${ND_NODES}'.split(','))
print('花名册收敛:', bool(wait_roster_online(entry_base(), n, 300)))
"
}
