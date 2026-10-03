# NexBoot 无盘启动系统术语

无盘客户机通过 iPXE 从镜像服务器挂载系统盘/数据盘启动。当前架构由 Go 单体负责引导、配置管理与存储操作，并内嵌 React Web 界面。本表保留的 `Nd*`、PNP 等旧实现名称仅供术语对照。

## Language

**系统盘 (System Disk)**:
客户机的启动盘，通过 `sanhook --drive 0x80` 挂载，最终 `sanboot` 从它启动。每台客户机必有且仅有一个。
_Avoid_: 启动盘

**数据盘 (Data Disk)**:
分组下配置的附加磁盘（实体 `NdGroupDisk`），每块有一个**盘符**（`mountPoint`，如 `D:`/`E:`）和绑定的镜像。一个分组可有零到多块数据盘。
_Avoid_: 附加盘

**驱动盘 (Driver Disk)**:
PNP 驱动的安装目标。**不是独立的物理盘**——其盘符 (`driverDiskLetter`) 必须等于某一块已有数据盘的盘符，表示"驱动安装到哪块数据盘上"。一个分组最多配置一个驱动盘（盘符 + 下载 URL `driverDownloadUrl`）。
_Avoid_: 独立驱动盘

**盘符 (Drive Letter)**:
Windows 盘符（如 `D`/`E`），代码中统一用 `normalizeDiskLetter` 归一化（去冒号、去空格、转大写）。数据盘的盘符存于 `NdGroupDisk.mountPoint`，驱动盘盘符存于 `NdGroup.driverDiskLetter`。

**超管机 (Super Admin Machine)**:
`terminalSuper == 1` 的客户机（`Configurations.isClientSuper(mac)` 判定）。本质是**母盘机/模板机**——其磁盘改动会**持久化回母镜像**（走 `superPrepareForClone`）；普通机器拿的是**临时克隆**（开机即焚）。PNP 驱动流程中，母盘机负责"固化"驱动，普通机只"套用"已固化的驱动，二者 iPXE 配置互斥。
_Avoid_: 管理机

## 存储模型

**镜像 (Image)**:
导入的原始系统盘,只读基线,写时复制(CoW)链的根,带初始还原点。客户机**不直接**用它启动。
_Avoid_: 母盘(口语可,正式文档用"镜像")

**配置 (Config)**:
镜像快照的一个克隆/分支。一个镜像可有多个配置;导入镜像时自动建一个名为 `default` 的配置。**客户机永远从某个"配置"启动**。
_Avoid_: 分支

**还原点 (Reduction / Restore Point)**:
配置上的一个快照,代表一个可启动的"存档点"。可创建/删除/合并。实体 `NdReduction`。
_Avoid_: 快照(快照是 ZFS 实现细节,对外说"还原点")、备份

**客户机克隆 (Client Clone)**:
从某配置的某还原点克隆出的、客户机独占可写卷。普通机 = 临时克隆(`CLIENT-<mac>`,开机即焚,且下线一段时间后由**离线克隆回收**主动收走);超管机 = 持久克隆(`SCLIENT-<mac>`,永不回收)。

**启动脚本烘焙 (Startup Script Bake)**:
Windows 客户机需要一份 GPO 启动脚本恢复默认网关/DNS(C-1 把网关挡在 iBFT 外)并应用数据盘盘符。这份脚本**对所有客户机字节相同**(per-client 数据由脚本运行时回调 API 取),所以在**镜像导入时、打初始快照之前**注入一次,下游配置/还原点/客户机克隆沿 CoW 链全部继承——而不是每台每次开机各挂载一次自己的克隆去写同样的内容。镜像记录存一个脚本版本号(`MountScriptVersion`),`/boot` 比对它决定能否跳过,**是字符串比较,不需要挂载**。版本为空或不匹配(存量镜像、服务端改了脚本、改了 API 端口)→ 退回每台注入,行为与烘焙前一致。
_已知边界_: 记录说「已烘焙」但有人从超管机里删掉了 GPO 文件再提交还原点 → 开机会错误跳过、该组丢失网关。这是「信记录不实地检查」的固有代价,换的正是不挂载。
_Avoid_: 为了判断「有没有脚本」去挂载客户机克隆

**离线克隆回收 (Offline Clone Reclaim)**:
客户机停止使用后回收其临时克隆(`CLIENT-<mac>`)。判据是**服务端观察到的 iSCSI 会话**而非猜测,且**会话断开 ≠ 关机**——链路抖动/交换机重启/客户机休眠都会短暂清空会话,所以离线后要熬过一段宽限期才动手。两个候选来源互补:终端记录的 `OfflineAt`(重启后仍在库里)+ 节点上的克隆清单对账(覆盖记录陈旧、终端已删、历史遗留孤儿)。由 `internal/control/assets/terminal_gc.go` 独占实现。回收与开机供给按 MAC 互斥(`internal/storage/local` 的 per-client 锁),否则会销毁开机路径刚克隆出的数据集。
_Avoid_: 靠心跳/客户机上报判断关机、离线即刻销毁

**离线卷会话 (Offline Volume Session)**:
在服务端本地挂载客户机克隆/镜像克隆的分区做离线读写——GPO 脚本注入、适配结果回读、镜像体检都走它。由 `internal/storage/local/winvol.go` 独占实现,不变量:挂载绝不泄漏(会话内 defer 释放 + teardown 前回收残留)。
_Avoid_: 在调用方手写 mount/umount

**载体 (Carrier)**:
池里承载某个领域实体的数据集或快照,以及它的名字。取名与认名是同一条规则的两个方向——镜像/配置/还原点要在池里获得一个 ZFS 接受的名字(ASCII;显示名归库,见「还原点」),而任何读池的代码要能从名字认出面前是什么(客户机克隆 / 超管机克隆 / 体检临时克隆 / 被顶替的配置 / 实体本身)。两个方向由 `internal/storage/layout.go` 独占实现,`Classify` 是认名的那一半。
_Avoid_: 在调用方拼 `"CLIENT-" + mac` 或用 `strings.HasPrefix` 自行判断名字属于什么

**池拓扑 (Pool Topology)**:
一次多步池内改写开始时读到的池状态,以及在其上「使其如此」地推进每一步。ZFS 没有事务,中断后唯一的前进方式是**重跑**,而盲目重跑会销毁上一次已经改名到位的数据集——所以先读拓扑、判断停在哪一阶段、再从该阶段续跑,每一步以「已经是目标状态即完成」表达(`ensureDestroyed`/`ensureRenamed`/`ensurePromoted`/…)。合并配置与超管机存盘都走它。由 `internal/storage/local/topology.go` 独占实现。
_Avoid_: 把 zfs 的「已存在/不存在/不是克隆」错误在调用方逐处容忍、按固定顺序盲目重放多步操作

**zvol 设备节点 (Zvol Device Node)**:
zvol 在内核侧的真实块设备 `/dev/zdN`(devtmpfs 直建)。**正确性永远不依赖 udev 的 `/dev/zvol` 符号链接**——开机风暴下 udev 事件队列滞后数秒,链接可能缺失、甚至指向已被回收复用的 minor(= 别的客户机的盘)。节点↔数据集映射、出现/释放等待、挂载枚举由 `internal/storage/zfs/zvoldev.go` 独占实现(核心原语 BLKZNAME ioctl)。
_Avoid_: readlink /dev/zvol 判断设备身份、udevadm settle 等设备就绪

## Relationships

- 一个 **镜像** 拥有一个或多个 **配置**(`default` 必有)
- 一个 **配置** 拥有零或多个 **还原点**
- 一台客户机开机时,从"分组选定的 **配置** + **还原点**"克隆出一个 **客户机克隆** 作为系统盘
- **超管机** 的改动可固化为该 **配置** 的新 **还原点**(母盘编辑)

## Flagged ambiguities

- **驱动盘、GPU 配置（重写中已弃用，2026-06-29）**：原系统有"驱动盘"(PNP 驱动装到某数据盘)和"GPU 配置下发"两条功能线。**新架构整体移除二者**——`driverDiskLetter`/`driverDownloadUrl`/`purepnp`/`puregpu`/`NdTerminalGpu` 及"驱动盘盘符必须取自数据盘"约束全部作废。本术语表中"驱动盘"条目仅供阅读旧代码时参考。
- **网关清零**：Windows iSCSI 路由可能将同网段流量绕到网关，因此仅对 Windows 镜像在 `sanhook` 前执行 `set netX/gateway 0.0.0.0`；Linux 镜像不清网关。Windows 启动后再由启动脚本恢复网关和 DNS。
