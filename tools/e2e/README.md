# 端到端功能矩阵

对着一台真实部署的服务器跑的六个矩阵。它们覆盖的是单元测试结构上够不到的东西：
ZFS 拒绝一个操作、LIO 导出一个 LUN、分区节点出现、镜像体检真的读到了 Windows 分区。
每一项在这里失败，都对应一个只有真机会暴露的缺陷类别。

| 脚本 | 覆盖 |
| --- | --- |
| `config_reduction_matrix.py` | 配置与还原点：新建/派生/合并/删除，正向与异常各一遍；还原点导出到服务器目录（H'） |
| `boot_matrix.py` | 开机链路：烘焙、克隆命名、数据盘 LUN 与盘符对齐、删终端回收、合并中断续跑、Windows 开机策略下发、按 MAC 取挂盘脚本、分组满员报错点名 |
| `install_pe_matrix.py` | ISO 安装 / WinPE / 机型 / 终端身份入口的服务端边界：导入目录外与符号链接逃逸、非安装盘 ISO、伪造令牌、不存在对象（真实安装与取证仍需客户机，记为跳过） |
| `driver_matrix.py` | 机型页驱动包：上传、INF 解析、禁用与删除 |
| `platform_matrix.py` | 用户增删改密与登录、告警从产生到确认到自动恢复、审计落库 |
| `pool_matrix.py` | 存储池：建池、加盘、换盘、读写缓存增删、销毁与盘的回收 |
| `volume_matrix.py` | 存储池详情：页面和 `zfs list` 逐项对账、各类别合计等于已用、读一次不改动池、10 秒缓存、读不到和拒绝的说法（只读，不留状态） |
| `import_delete_matrix.py` | 删除导入文件：不该删的删不掉、页面马上更新、审计带文件名、备机跟着删、旧标记轮转后空间真的回来（只动自己上传的 64 MiB 文件） |
| `delete_standby_down_matrix.py` | 一台备机停服时删镜像：刚停时拒绝并点名、什么都没动，另一台照常追平；心跳过期后能删，另一台跟着删；停掉的回来后增量追平，不触发整份重建（**破坏性**：停一台备机几分钟，需 ND_NODES、ND_CLUSTER_TOKEN） |
| `backup_matrix.py` | 本地备份：整库一条流、DB 副本随流、合并/超管保存后增量自愈、基准丢失退全量（需 ND_BACKUP_POOL 与 ND_WINDOWS_SOURCE） |
| `ha_matrix.py` | 双机热备：VIP 漂移计时、故障切换/回归/计划切换、隔离下激活门、双程数据存活（需 ND_NODE_A/B、ND_VIP、ND_CLUSTER_TOKEN） |
| `cluster_matrix.py` | 三节点集群：节点注册、分组放置到存储节点、故障回退与恢复、quorum 激活门、清理路由（需 ND_NODE_A/B/C、ND_VIP、ND_CLUSTER_TOKEN） |
| `failover_matrix.py` | 带客户机的异常切换：整机断电、网卡断链、切换正确性、旧主带未复制数据复活、隔离防脑裂、连续切换、复制中断追平 |
| `cluster_ops_matrix.py` | 集群管理面：内部端点的认证边界、池身份按节点、集群容量聚合、按节点看盘、跨节点重启/看日志、点错节点必须拒绝（按 `ND_NODES` 的节点数自适应，单机跑降级断言） |
| `cluster_join_matrix.py` | 界面入盟：join-config 的认证与内容、成员不可纳管/再加入/建集群、discover；复制残留（recv-* 临时集、-rebuilding-）回收而 -diverged- 保留；超管机落在写入者不受分组绑定影响；`ND_JOIN_DESTRUCTIVE=1` 时把一台备机清成单机再经界面重新入盟（**破坏性**） |
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
- 池里有导入目录；`boot_matrix.py` 还需要一个**能通过体检的真实 Windows 镜像源文件**
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
export ND_WINDOWS_SOURCE=/tank/imports/xxx.gzip # boot 矩阵需要

python3 tools/e2e/config_reduction_matrix.py
python3 tools/e2e/boot_matrix.py
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
（`ND_USER`/`ND_SSH_HOST`/`ND_SSH_USER`/`ND_POOL`/`ND_IMPORT_DIR`）。导入目录固定为写入者存储池上的
`<池挂载点>/nd/files/imports`，不设 `ND_IMPORT_DIR` 时从 `/api/runtime` 读取。

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

### 网卡 PNP 半自动真机矩阵（071）

先从待测还原点派生独立的 `e2e-` 配置，给独立 `e2e-` 分组绑定该配置；测试终端名称也必须以 `e2e-` 开头。脚本会拒绝现场配置和在线/超管终端，不自动清理资产。

```sh
export ND_PNP_CONFIG=<UEFI测试配置ID>
# 有真实 MBR 镜像时填写，未填时 BIOS 只验证 PE，报告不会宣称 BIOS 能进桌面。
export ND_PNP_BIOS_CONFIG=<BIOS测试配置ID>
python3 tools/e2e/nic_pnp_matrix.py
```

每个机型按提示设置两个不同 MAC；第二台正常网络启动，按客户机详情的「预计下次启动」核对：可计算或借用实例号的 VMware 机型直接生成身份层，物理机先自动进入 WinPE 取证；借用实例号一次未取得成功回报后也应转入取证。不使用手动取证入口替代验收。每台检查三次桌面、三次新开机回调、身份层和五分钟在线，人工补录数据盘日志与 NOPIN 日志证据位置，结果保存到 `nic-pnp-results.json`。脚本跑完仍不代表完整 071：PCI 槽位、切点/超管保存、失败注入、关机后 SYSTEM 驱动核对、跨节点残留和一致性检查另行留证。

### 网卡生成规则变更后的补充验收

本地单元测试不能代替真实注册表和 Windows 开机。测试版本部署后，使用上述隔离的 `e2e-` 配置和客户机完成以下检查，保留原还原点用于回滚：

1. 记录测试二进制版本、`internal/winreg/generate.go` 的 `GeneratorVersion`、父还原点、驱动包版本、客户机 MAC 和 iPXE 实际网卡 ID。
2. 用 vmxnet3 客户机（例如 22 号机）分别测试镜像原生网卡、同型号新 MAC 的自动模板、已有 WinPE 适配结果的 MAC 推算路径；E1000/E1000E 另做跨型号验证。每次核对预览与服务端实际启动选择一致。
3. 关机后只读导出测试身份层 SYSTEM，核对设备实例、HardwareID、MAC、GUID/LUID、DHCP 和 `InfPath`；第三方包必须对应镜像已发布的 `Windows/INF/oemN.inf`，未登记的上传包应提示先用 WinPE/DISM 适配。
4. 每条生成路径连续进桌面三次，检查每轮新的成功回调，最后保持 iSCSI 会话至少 10 分钟。没有桌面、回调和会话证据不得标记通过。
5. 隔离环境中验证借用实例未回报一次即取证、规则/驱动变化后的离线预建，以及多台同机型同时首次启动；记录请求耗时和节点 CPU。单元测试的 hivex 调用计数不能代替这里的并发性能验收。

装有 hivex 的 Linux 测试机可先用从**同一镜像**取出的 SYSTEM、DriverStore 包和已发布 INF 做离线验证；测试只修改临时副本，不修改原 hive：

```sh
NEXBOOT_TEST_SYSTEM_HIVE=/path/to/copied/SYSTEM \
NEXBOOT_TEST_DRIVER_DIR=/path/to/copied/DriverStore/package \
NEXBOOT_TEST_PUBLISHED_INF=/path/to/copied/Windows/INF/oem0.inf \
NEXBOOT_TEST_INSTANCE='PCI\VEN_15AD&DEV_07B0&SUBSYS_07B015AD&REV_01\005056FFFF2EF65000' \
go test ./internal/winreg -run '^TestApplyGeneratedOnARealHive$' -v -count=1
```

输出 `SKIP` 表示缺工具或样本，不能记为验证通过；离线写入通过也不能代替上面的 Windows 开机验收。

## ISO 安装验证

这部分必须在隔离测试池和可恢复客户机上执行，不能用单元测试代替真实安装。安装矩阵：Windows 10 x64、已修复 iSCSI 问题的 Windows 11 x64、Ubuntu 24.04 LTS（ISO 必须含离线组件）、CentOS 7 DVD，以及 RHEL/Rocky/AlmaLinux 8/9 完整 DVD，均为 x64 官方 ISO + UEFI/GPT；自动化回归不代表这些版本都已实机通过。每种系统至少覆盖一次空环境制作和一次另一 MAC 的普通启动。

1. 服务器安装最新版及打包依赖，创建测试存储池；将 ISO 上传或放进设置的导入目录。服务器和客户机均断开外网，验证安装只使用 ISO 内组件；关闭 Secure Boot，设置 UEFI PXE。
2. 客户机接入一块内容已知的本地测试盘，记录分区、EFI 文件和整盘 SHA256；测试结束必须再次核对。不要用不可恢复的业务盘。
3. 镜像管理 → 从 ISO 创建 → 选择系统版本、池、账户、容量 → 填写关机客户机 MAC 和预留 IP。空环境不得要求已有镜像、分组或终端。等待阶段不得显示安装百分比或就绪。
4. 启动客户机，检查任务进入安装、首次系统启动、等待断开和保存阶段。安装多次重启应使用同一 INSTALL 卷；安装中异常重启必须停止，不能再次 clean。Windows 缺网卡驱动时，在驱动包上传匹配驱动后重新创建；检查任务 PE/DISM 日志。
5. 自动关机后等状态变为“等待无盘启动验证”。此时 `/api/images`、分组选择器中仍不得出现候选镜像。再次 PXE，从普通 CLIENT 克隆启动；仅本次 boot_id + 操作系统回报 + 活跃 iSCSI 会话才能发布为就绪。
6. 将生成的镜像分配给新分组，用不同 MAC 客户机连续普通开机两次。Windows 应无 OOBE 循环、0x7B；Linux 应从当前 iBFT target 启动，没有登录制作盘旧 target，machine-id/SSH host keys 与制作机及其它客户机不同。关机后原分组配置不变。
7. 故障矩阵：准备中服务重启；安装中断电；保存中重启；验证前服务重启；重复和旧 attempt 回报；取消时仍在线；安装期间删除池/修改终端；ISO 损坏、越界路径、盘序列号不唯一、导入目录/池空间不足。失败须可诊断，原卷保留可恢复，候选不得被目录恢复自动收养。
8. 下载全部安装日志并检查密码、SHA512 密码摘要、CHAP 密码、会话令牌均未泄漏。就绪/取消后临时安装介质与应答文件已清理，原始用户 ISO 不删除；核对第 2 步本地盘内容未变。

### RHEL 系 ISO 补充检查

- 使用完整 DVD：CentOS 7 为根目录 Packages/repodata，8/9 为 BaseOS/AppStream。缺少 iSCSI、dracut、curl 等必需包时，应在识别阶段明确拒绝，不进入会格式化磁盘的步骤。
- 安装期间仅有服务端指定的 LUN 0 可写；LUN 1 是只读 ISO。故意制造序列号不匹配时必须停止，本地测试盘分区和内容保持不变。
- 检查 Kickstart 安装、首次启动、保存、普通启动验证四阶段；任务详情能下载 `install.log` 与 `anaconda.log`，日志不可包含口令、密码摘要或会话令牌。
- 安装后的 initramfs 含 iSCSI 和网络模块，启动条目含 `rd.neednet=1 rd.iscsi.firmware=1 ip=ibft`；不得遗留制作机专属的 IP、MAC、旧 iSCSI target 或安装 ISO 依赖。
- 为第二个 MAC 分配同一镜像，确认独立身份、普通开机成功与 Linux 数据盘挂载；确认镜像体检识别 grub2/EFI/BLS 的 dracut 参数。记录具体 ISO 校验值和版本，不以一个发行版的结果代替其他发行版验收。
