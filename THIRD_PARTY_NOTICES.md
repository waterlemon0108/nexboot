# 第三方组件声明

NexBoot 本身采用 [GNU AGPLv3](./LICENSE) 授权。随程序一起分发的第三方组件保留各自的协议，列在下面。

## 随二进制分发的独立程序

以下文件作为独立程序打包在 `internal/boot/tftpassets/files/`，运行时经 TFTP 发给客户机执行，与 NexBoot 的代码不链接，各自适用自己的协议。

| 文件 | 项目 | 版本 | 协议 | 源码 |
|---|---|---|---|---|
| `undionly.kpxe` | [iPXE](https://ipxe.org) | Ubuntu 22.04 源码包 `1.21.1+git-20220113.fbbdc3926-0ubuntu1` | GPL-2.0-or-later（部分文件附 UBDL） | Ubuntu 22.04 `apt-get source ipxe`；未修改源码，构建方法见 [`scripts/build-undionly.sh`](scripts/build-undionly.sh)（不内嵌脚本） |
| `snponly.efi` | [iPXE](https://ipxe.org) | 同上 | GPL-2.0-or-later（部分文件附 UBDL） | 同上，构建目标为 `bin-x86_64-efi/snponly.efi` |
| `wimboot` | [wimboot](https://ipxe.org/wimboot) | v2.8.0 | GPL-2.0-or-later | <https://github.com/ipxe/wimboot/tree/v2.8.0> |

这些组件的协议全文见 <https://www.gnu.org/licenses/old-licenses/gpl-2.0.txt>。如需上述文件对应版本的完整源码，可按上表获取，或联系项目维护者索取。

## 编译进二进制的 Go 依赖

| 协议 | 模块 |
|---|---|
| MIT | github.com/dustin/go-humanize、github.com/go-chi/chi/v5、github.com/golang-jwt/jwt/v5、github.com/jackc/pgpassfile、github.com/jackc/pgservicefile、github.com/jackc/pgx/v5、github.com/jackc/puddle/v2、github.com/mattn/go-isatty、github.com/mfridman/interpolate、github.com/ncruces/go-strftime、github.com/pressly/goose/v3、go.uber.org/multierr |
| BSD | github.com/google/uuid、github.com/remyoudompheng/bigfft、golang.org/x/sync、golang.org/x/sys、golang.org/x/text、modernc.org/libc、modernc.org/mathutil、modernc.org/memory、modernc.org/sqlite |
| Apache-2.0 | github.com/sethvargo/go-retry |

## 打包进前端的依赖

| 协议 | 包 |
|---|---|
| MIT | react、react-dom |

## 运行时调用的系统程序

dnsmasq、targetcli-fb、ZFS（zfsutils-linux）、keepalived、qemu-utils 由操作系统包管理器安装，NexBoot 只在运行时调用，不随本项目分发。
