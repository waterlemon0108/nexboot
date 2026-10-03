#!/bin/sh
# keepalived 状态变更时通知 ndiskless 切换角色。只触发不等待：阻塞 notify 会拖累 VRRP 状态机。
# master：拿到 VIP，重启为 active（启动路径会同步并重启 dnsmasq）。
# backup/fault/stop：失去 VIP，重启为 standby（启动路径会停 dnsmasq、清导出）。
STATE="$1"
TOKEN="$(cat /etc/ndiskless/cluster-token 2>/dev/null)"
BASE="http://127.0.0.1:@API_PORT@"
LOG="logger -t ndiskless-ha"

case "$STATE" in
  master)
    $LOG "keepalived MASTER: activating"
    curl -sf -m 5 -X POST -H "Authorization: Bearer $TOKEN" "$BASE/internal/ha/activate" >/dev/null 2>&1 &
    ;;
  backup|fault|stop)
    $LOG "keepalived $STATE: stepping down"
    curl -sf -m 5 -X POST -H "Authorization: Bearer $TOKEN" "$BASE/internal/ha/standby" >/dev/null 2>&1 &
    ;;
esac
exit 0
