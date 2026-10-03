#!/bin/bash
# 分级套件：同一批机器依次组成单机 → 双机 → 三机，每级跑对应矩阵。
# 破坏性：阶段 1 会 wipe 三台并重装（保留数据池，清掉库、配置、keepalived 和池内 DB 副本），只对测试机跑。
. "$(dirname "$0")/_common.sh"
: "${ND_HOSTS:?env 里缺 ND_HOSTS}"
: "${ND_POOL_DISK:?env 里缺 ND_POOL_DISK}"
echo "  即将 wipe 并重装: $ND_HOSTS"
exec python3 -u topology_suite.py
