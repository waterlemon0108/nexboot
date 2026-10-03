# 端到端功能矩阵

对着一台真实部署的服务器跑的六个矩阵。它们覆盖的是单元测试结构上够不到的东西：
ZFS 拒绝一个操作、LIO 导出一个 LUN、分区节点出现、镜像体检真的读到了 Windows 分区。
每一项在这里失败，都对应一个只有真机会暴露的缺陷类别。

| 脚本 | 覆盖 |
| --- | --- |
| `config_reduction_matrix.py` | 配置与还原点：新建/派生/合并/删除，正向与异常各一遍（38 项） |
| `boot_matrix.py` | 开机链路：烘焙、克隆命名、数据盘 LUN 与盘符对齐、删终端回收、合并中断续跑 |
| `adaptation_matrix.py` | 超管机离线适配：克隆建出、适配脚本与 GPO 注册落盘、重复注入需显式覆盖 |
| `driver_matrix.py` | 驱动中心：上传驱动包（解析 .inf）、组合驱动集、归档下载、注入超管机 |
| `platform_matrix.py` | 用户增删改密与登录、告警从产生到确认到自动恢复、审计落库 |
| `pool_matrix.py` | 存储池：建池、加盘、换盘、读写缓存增删、销毁与盘的回收 |
| `backup_matrix.py` | 本地备份：整库一条流、DB 副本随流、合并/超管保存后增量自愈、基准丢失退全量（需 ND_BACKUP_POOL 与 ND_WINDOWS_SOURCE） |
| `ha_matrix.py` | 双机热备：VIP 漂移计时、故障切换/回归/计划切换、隔离下激活门、双程数据存活（需 ND_NODE_A/B、ND_VIP、ND_CLUSTER_TOKEN） |
| `cluster_matrix.py` | 三节点集群：节点注册、分组放置到存储节点、故障回退与恢复、quorum 激活门、清理路由（需 ND_NODE_A/B/C、ND_VIP、ND_CLUSTER_TOKEN） |
| `failover_matrix.py` | 带客户机的异常切换：整机断电、网卡断链、切换正确性、旧主带未复制数据复活、隔离防脑裂、连续切换、复制中断追平 |
| `cluster_ops_matrix.py` | 集群管理面：内部端点的认证边界、池身份按节点、集群容量聚合、按节点看盘、跨节点重启/看日志、点错节点必须拒绝（按 `ND_NODES` 的节点数自适应，单机跑降级断言） |
| `balance_matrix.py` | 客户机放置：自动均衡摊开、上次落点粘住、显式绑定压过均衡、解绑回到均衡、节点掉线后改投健康节点 |
| `ha_recovery_matrix.py` | 高可用自愈与人工救场：持 VIP 却是备机→自行激活、两台同时持 VIP（分区）→不得抢、不持 VIP 却是主机→退位、强制激活不跳过防双写栅栏 |
| `topology_suite.py` | 拓扑分级驱动：把同一批机器依次拉成单机 → 双机 → 三机，每一级跑该级该有的矩阵（**破坏性**，见下） |

## 离线配置检查

以下检查不连接服务器，验证 `reset.py`、`wait_settled.py` 缺少 `ND_VIP` 时提前拒绝，以及按完整地址识别自定义 VIP：

```sh
python3 tools/e2e/test_environment.py
```

## 前置

- 目标机已部署并可登录，本机可 `ssh` 到它（脚本用 `sshpass`，需先 `brew install sshpass` 或 `apt install sshpass`）
- 池里有导入目录；`boot_matrix.py` 与 `adaptation_matrix.py` 还需要一个**能通过体检的真实 Windows 镜像源文件**
  （zfs 流最快，几十秒导完）。64M 的假 raw 过不了体检，建组会被阻断。

## 跑法

### 集群矩阵：用 `run/` 下的编排，别手拼环境变量

集群那几个矩阵要跑在**集群内部的某台节点上**（节点之间要能直连，从外面经端口转发
够不着），而且「哪台当 A、哪台当 B」不能写死——三台都参选 VRRP 之后，虚 IP 可能
落在任何一台，而 `ha_matrix` 要在 A 和 B 之间做计划切换，A 必须是**此刻真正在写**
的那台。这些知识都编码在 `run/` 里了：

```sh
cd tools/e2e/run
cp env.example env && vi env      # 按现场改：三台地址、VIP、口令、池名、镜像源
./sync.sh                          # 把 harness 铺到**每台**节点（不是只铺一台）
# 然后到任一节点上，进入 env 中 ND_E2E_DIR 指定目录的 run/ 子目录：
./all.sh                           # 五套回归
./one.sh backup_matrix.py          # 单跑一个，排查时用
./topology.sh                      # 分级：单机 → 双机 → 三机（**破坏性**，会重装）
```

`env` 含口令，已在 `.gitignore` 里；只提交 `env.example`。

### 没有真实 Windows 镜像时

`boot_matrix` / `backup_matrix` 需要一个**能过导入体检**的镜像源。手头没有真实
镜像时用夹具造一个最小的（1G 表观、实占几 MB）：

```sh
sudo tools/e2e/fixtures/mkwin.sh /var/lib/ndiskless/imports/winmin.raw 1G
```

体检的阻断项实际只有两条（`Windows/System32` 和 `drivers/msiscsi.sys`），夹具把
它们连同网卡 INF、BCD 一并放进去。**它不代表真实 Windows 能启动**——但那两个矩阵
断言的是流、增量、克隆命名、超管保存这些，与镜像内容无关。要验真实启动得用真镜像。

缺镜像源时两个矩阵会**整段跳过并说明原因**，不会空跑成一串指向别处的失败。

### 单机矩阵：直接跑

```sh
export ND_BASE=http://192.168.124.56:18080
export ND_PASSWORD='...'                       # 也用作 SSH 密码，除非另设 ND_SSH_PASSWORD
export ND_WINDOWS_SOURCE=/tank/imports/xxx.gzip # boot / adaptation 两个矩阵需要

python3 tools/e2e/config_reduction_matrix.py
python3 tools/e2e/boot_matrix.py
python3 tools/e2e/adaptation_matrix.py
python3 tools/e2e/driver_matrix.py
python3 tools/e2e/platform_matrix.py
python3 tools/e2e/pool_matrix.py     # 需要四块空闲块设备，见下
```

## 造测试盘（存储池矩阵）

产品只把 lsblk 的 `TYPE=disk` 当候选盘，loop 回环文件不算。没有整块空闲物理盘时，
用内核自带的 scsi_debug 造四块**互相独立**的假盘：

```sh
modprobe scsi_debug dev_size_mb=1024 add_host=4 num_tgts=1 per_host_store=1
```

三个参数都不能省：`num_tgts=4` 会让四个 target 共用同一份存储，ZFS 会报
「one or more vdevs refer to the same device」；`per_host_store=1` 才让每个 host
拿到自己的后端存储。跑完 `rmmod scsi_debug` 收走，重新 modprobe 即得干净的盘。

全部通过时退出码为 0，失败项会在末尾再列一遍。其余可调项见 `ndapi.py` 顶部注释
（`ND_USER`/`ND_SSH_HOST`/`ND_SSH_USER`/`ND_POOL`/`ND_IMPORT_DIR`）。

## 多节点矩阵的跑法

节点之间要能直连，所以矩阵在**集群内部**跑（把 `tools/e2e/` 复制到任一节点上执行），
不是从开发机经端口转发打进去。

编排在 [`run/`](run/) 里，别再手拼环境变量：变量有十来个，漏一个的后果往往是
**静默的**（下面那条就是例子）。

```sh
cd tools/e2e/run && cp env.example env && vi env
./sync.sh                       # 铺到每台节点
# 到任一节点，进入 ND_E2E_DIR 指定目录的 run/ 子目录，再执行 ./all.sh
```

**`ND_PASSWORD` 与 `ND_SSH_PASSWORD` 要分清。** 混用时 SSH 会静默失败，而失败的形式是
「读到空」——断言据此给出的是一个看起来成立的错误结论，比直接报错更难发现。
`env.example` 把两者分开列着，就是为了不让人省掉这一步。

## 拓扑分级套件（破坏性）

`topology_suite.py` 按产品文档的升级路径把三台机器依次拉成单机 → 双机 → 三机，
每一级跑该级该有的矩阵。这么走而不是在三节点上「模拟」三种形态，是因为产品只支持
往上加节点；而升级路径本身正是最容易出事的地方——第二台加入时，花名册里只有第一台
的那一行曾被当成「本机的旧身份」。

```sh
./topology.sh                       # 在节点的 ND_E2E_DIR/run 中运行；ND_STAGES 可只跑某几级
```

每一级跑的矩阵：单机跑集群管理、负载均衡、**开机链路**、**本地备份**；双机加上
双机热备与带客户机的异常切换；三机再跑一遍开机链路（**「超管保存必须落在写入者上」
这条只有多节点时才验得了**）与三节点集群矩阵。

开机链路此前不被任何套件引用、只能手动单跑，于是那个「超管保存落在备机上、快照被
复制流回滚」的缺陷躲过了很多轮——把它接进来正是为了不再发生这种事。

它会在每台上**清掉目录库、配置和 keepalived 后重装**。数据池留着不动：镜像、配置、
还原点住在池里，服务启动时会重新认领回来；分组、终端、用户恢复不了，而它们恰好是
每个矩阵自己建、自己清的。

## 两条硬规矩

**不要对生产池运行。** 六个脚本都会删除并重建自己的测试镜像、分组、终端、驱动包与用户；
`pool_matrix.py` 还会建/毁自己的存储池（只碰它自己建的那个，绝不碰已有池）。

**读结果时当心三个假失败**（脚本里已经绕开，改写时别退回去）：

- 任务要在一段历史里找，不能只看最新一条——导入镜像后紧跟着自动体检任务，
  `limit=1` 会把成功的导入判成失败。
- 删终端走 ID（`/api/terminals/terminal-AABBCC112233`），传 MAC 得 404 且静默，
  看起来像「克隆没回收」。
- 列表接口与 `/boot/data-disks` 都返回 `{items:[...]}` 包装，和裸数组比会假失败。
- 归档/导出这类返回文件的接口要用 `get_bytes()`，`call()` 按文本解码会在 zip 的第一个字节上炸。
- 告警的 `Resource` 是镜像 **ID** 而不是显示名（导入时中文名会折成 ASCII），按显示名匹配会永远找不到。
- 条件不具备时用 `skip()` 而不是记成通过——记成通过就是矩阵在虚报自己的覆盖。

矩阵通过后，还需用真实客户机手动确认能进入桌面、数据盘盘符可见，以及
`adapt.ps1` 在 Windows 内确实装上了驱动；这些结果不能由接口断言代替。
