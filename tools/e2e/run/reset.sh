#!/bin/bash
# 把集群带回干净起点。跑全量之前用它，或者怀疑环境脏了随时用。
#   ./reset.sh           复位
#   ./reset.sh --check   只检查不动手
. "$(dirname "$0")/_common.sh"
exec python3 -u reset.py "$@"
