#!/bin/bash
# 单跑一个矩阵：./one.sh backup_matrix.py [矩阵参数...]，参数原样透传，如 ./one.sh failover_matrix.py A4。
# 不要直接用 python3 跑：集群令牌由 _common.sh 解析，绕过会导致 /internal/ 全部 401。
. "$(dirname "$0")/_common.sh"
M=${1:?用法: one.sh <matrix.py> [矩阵自己的参数...]}
shift
[ -f "$M" ] || { echo "没有这个矩阵: $M" >&2; exit 2; }
case "$M" in
  ha_matrix.py|failover_matrix.py)
    # 这两个要在当前写入者和另一台备机之间切换，不能写死
    read -r W O <<< "$(writer_and_other)"
    echo "  A=$W B=$O"
    exec env ND_NODE_A="$W" ND_NODE_B="$O" python3 -u "$M" "$@" ;;
  cluster_matrix.py)
    IFS=',' read -r N1 N2 N3 <<< "$ND_NODES"
    exec env ND_NODE_A="$N1" ND_NODE_B="$N2" ND_NODE_C="$N3" python3 -u "$M" "$@" ;;
  *)
    # ndapi 系矩阵读 ND_BASE，env 里没有，不补会连到默认的 127.0.0.1:18080。
    # 取当前写入者：这些矩阵要做导入、备份等写操作。
    if grep -q "^from ndapi" "$M"; then
      read -r W _ <<< "$(writer_and_other)"
      echo "  ND_BASE=$W（当前写入者）"
      exec env ND_BASE="$W" python3 -u "$M" "$@"
    fi
    exec python3 -u "$M" "$@" ;;
esac
