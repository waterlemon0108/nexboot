# 打包

把静态二进制打成 `.deb`，再连依赖打成断网可装的离线包。

## 命令

```sh
make deb        # dist/ndiskless_<版本>_amd64.deb
make offline    # dist/nexboot-offline-ubuntu24.04-amd64.tar.gz（deb + 47 依赖 + install.sh）
```

版本号从 `git describe` 来（同 `--version`）；deb 版本把 tag 后的 `-N-g<hash>` 转成
`~N-g<hash>`，好让开发构建排在正式 tag 之下。

## 两条硬约束

1. **必须在 Linux 上跑**：`dpkg-deb` 只在 Debian/Ubuntu 有，本机（mac）跑不了。
   在 Ubuntu 或 CI 上执行。
2. **`make offline` 必须在与目标同版本的 Ubuntu 上、且这台能联网**：依赖 `.deb` 是
   按发行版拉取的，24.04 的包不能拿去装 22.04。构建机可以是办公网机器，不必是机房里的。
   如果构建机的 apt 源够不到某个包（比如镜像没同步 security 更新），`offline-bundle.sh`
   会**明确报告缺哪个**并说包不完整——不会静默产出一个装不上的包。

## 包里放什么、不放什么

deb 只管**文件**：

```
/opt/ndiskless/ndiskless               二进制
/opt/ndiskless/scripts/install-go.sh   配置逻辑
/opt/ndiskless/deploy/*                unit 源 + keepalived 模板/脚本
/etc/systemd/system/ndiskless.service  规范 unit（就是 deploy/ 那份）
/usr/sbin/ndiskless-configure          → install-go.sh --provisioned 的包装
```

deb **不选池、不写 env、不启动**——那些要现场决策，是 `ndiskless-configure` 的事。
`postinst` 只 `daemon-reload`；`prerm` 在卸载时 stop+disable。

`Depends:` 声明了运行依赖（dnsmasq / qemu-utils / targetcli-fb / zfsutils-linux /
keepalived / curl）。ZFS 内核模块随 `zfsutils-linux` 来（Ubuntu 预编译，不用 dkms）。

## 验证状态

真机 Ubuntu 24.04（`.56`）验证过：deb 构建、结构（8 文件路径正确）、版本注入、
`Depends`、offline-bundle 抓齐 47 依赖闭包并对够不到的包明确报错。

**未在真机跑完的**：`dpkg -i` → `ndiskless-configure` → 启动的完整生命周期。它会改动
机器状态（`prerm` 卸载时停服务），留给干净测试 VM / `topology_suite` 覆盖，不在带手工
配置的验证机上做。
