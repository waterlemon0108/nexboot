#!/bin/bash
# 九套回归：集群管理 / 负载均衡 / 三节点集群 / HA 自愈 / 双机热备 / 超管与数据盘发布 / 同镜像并发 / 集群备份 / 目录保全。
# 跑在集群内部的某台节点上。用法：ND_ENV=... ./all.sh
. "$(dirname "$0")/_common.sh"

# 先复位到干净起点，否则上一轮的残留会在这一轮表现成新缺陷。
# ND_SKIP_RESET=1 可跳过，此时出现失败要先怀疑残留而不是产品。
if [ "${ND_SKIP_RESET:-0}" != "1" ]; then
  echo "##### 0 复位到干净起点 #####"
  if ! python3 -u reset.py; then
    echo "!! 复位没达成，不往下跑了——脏环境上的结论不可信"
    exit 2
  fi
fi

wait_roster
read -r WRITER OTHER <<< "$(writer_and_other)"
echo "  ha_matrix 参数: A=$WRITER B=$OTHER"

# 退出码取 PIPESTATUS[0]，$? 是 tail 的，恒为 0。
# 运行期间关掉 errexit：pipefail 会带出矩阵的非零退出码，set -e 会静默中止后面所有套件，
# 看起来像卡死。失败应如实汇报，而不是终止。
run() { # 序号 标题 脚本 [额外环境]
  local no=$1 title=$2 script=$3; shift 3
  echo "##### $no $title #####"
  # 每套开跑前等复制追平，否则上一套的切换/断电会让下一套的目录比对超时失败。
  # 等不到也照跑，但先提示，免得把后面的失败当成产品问题。
  python3 -u wait_settled.py 600 || echo "!! 起点不干净，下面这一套的结论要打折扣"
  set +e
  env "$@" timeout 2400 python3 -u "$script" 2>&1 | tee "/tmp/e2e-$no.log" | tail -40
  local rc=${PIPESTATUS[0]}
  set -e
  echo "RC$no=$rc"
}

run 1 "集群管理（三机）" cluster_ops_matrix.py
run 2 "负载均衡（三机）" balance_matrix.py
IFS=',' read -r N1 N2 N3 <<< "$ND_NODES"
run 3 "三节点集群"       cluster_matrix.py "ND_NODE_A=$N1" "ND_NODE_B=$N2" "ND_NODE_C=$N3"
run 4 "HA 自愈"          ha_recovery_matrix.py
run 5 "双机热备"         ha_matrix.py "ND_NODE_A=$WRITER" "ND_NODE_B=$OTHER"
# 超管机放在切换类用例之后：它自带临时配置并自行清理，只需要有一个写入者。
run 6 "超管与数据盘发布" super_matrix.py "ND_BASE=http://$ND_VIP:8080"
# 只动自己新建的空白数据盘镜像，几十秒，不留状态。
run 7 "同镜像并发"       concurrency_matrix.py "ND_BASE=http://$ND_VIP:8080"
# 临时改备份配置、做一次计划切换，结束时还原并收走自己留下的副本。
run 8 "集群备份"         backup_cluster_matrix.py
# 放最后：它会在一台备机上制造分叉，那台要整体重建目录（几分钟）。
run 9 "目录保全与可见性" catalogue_matrix.py "ND_BASE=http://$ND_VIP:8080"
echo ALL-DONE
