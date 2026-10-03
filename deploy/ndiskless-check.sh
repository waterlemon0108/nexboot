#!/bin/sh
# keepalived 健康探针，是否该持有 VIP 由 /healthz 判断。「连不上」和「答不健康」要分开：
#   200          健康，持有 VIP。
#   其它 HTTP 码 服务自认不健康，立刻让出 VIP。
#   连不上       可能正在切换角色（写标记后退出、由 systemd 拉起，有两三秒无应答）。
#                判成故障会形成「切换→探针失败→再切换」的振荡，所以切换标记 15 秒内仍放行。
URL=http://127.0.0.1:@API_PORT@/healthz
SWITCH=/run/ndiskless.switching
WINDOW=15

# keepalived 超时会杀脚本并判失败，最坏路径须在预算内：1 + 0.5 + 1 = 2.5s < timeout 5s。
code=$(curl -s -o /dev/null -w '%{http_code}' -m 1 "$URL" 2>/dev/null)
case "$code" in
  200) exit 0 ;;
  000) ;;        # 连不上，下面判断是不是切换窗口
  *)   exit 1 ;; # 服务明确说自己不健康
esac

if [ -f "$SWITCH" ]; then
  now=$(date +%s)
  stamped=$(stat -c %Y "$SWITCH" 2>/dev/null || echo 0)
  if [ $((now - stamped)) -lt "$WINDOW" ]; then
    # 多数切换一两秒内就重新监听，再探一次。
    sleep 0.5
    [ "$(curl -s -o /dev/null -w '%{http_code}' -m 1 "$URL" 2>/dev/null)" = "200" ] && exit 0
    exit 0   # 仍在窗口内：等下一轮探测，别为一次重启搬走 VIP
  fi
fi
exit 1
