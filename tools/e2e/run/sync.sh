#!/bin/bash
# 把 harness 铺到每台节点：矩阵在集群内部跑，topology_suite 也会在 HOSTS[0] 上调起其它矩阵。
# 用法：在开发机上跑 ./sync.sh [ssh端口...]，默认按 env 里的 ND_NODES 走 22。
. "$(dirname "$0")/_common.sh"
DEST=${ND_E2E_DIR:-/home/${ND_SSH_USER}/e2e}
hosts() { echo "$ND_NODES" | tr ',' '\n' | sed -E 's#^https?://##; s#:[0-9]+$##'; }
for h in $(hosts); do
  echo -n "  $h: "
  sshpass -p "$ND_SSH_PASSWORD" ssh -o StrictHostKeyChecking=no "$ND_SSH_USER@$h" "mkdir -p $DEST $DEST/run" \
    && sshpass -p "$ND_SSH_PASSWORD" scp -q -o StrictHostKeyChecking=no "$E2E"/*.py "$ND_SSH_USER@$h:$DEST/" \
    && sshpass -p "$ND_SSH_PASSWORD" scp -q -o StrictHostKeyChecking=no "$E2E"/run/*.sh "$E2E"/run/env "$ND_SSH_USER@$h:$DEST/run/" \
    && echo OK || echo 失败
done
