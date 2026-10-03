// Package control 是用例编排层（系统"大脑"），按功能域拆为子包，本包只保留
// 跨域编排的开机链路（boot.go：iPXE 脚本、开机克隆准备），它组合 assets 与
// adapt 完成一次完整开机。
//
// 子包与依赖方向（单向，无环）：
//
//	errs/      错误分类税则：NotFound/Invalid/Conflict… 构造器与 IsNotFound，
//	           供 api 层统一映射状态码，所有子包依赖它
//	tasks/     异步任务基础设施（Runner：建任务行、同步/后台执行、失败落库）
//	assets/    核心资产：镜像 image → 配置 config → 还原点 reduction 的 CoW 链，
//	           分组 group / 数据盘 group_disk / 客户机 terminal（含批量导入导出）、
//	           镜像体检 image_health、分组 IP 窗口 addresspool、删除引用保护 references
//	adapt/     硬件适配：离线驱动注入 inject（auto-adapt.ps1、
//	           mount-disks.ps1）、驱动中心 driver
//	ops/       存储运维：存储池 pool（zpool/缓存盘）、本地备份 backup（zfs send/recv）
//	platform/  平台管理：登录 auth、用户 user、审计 audit、系统设置 settings、
//	           systemd 服务 service、告警 alarm（规则依赖 ops 的池/备份状态）
//
//	依赖：control(boot) → assets, adapt；adapt → assets；platform → ops；
//	      所有子包 → errs, tasks（按需）。反方向 import 均属违规。
//
// 领域术语（镜像/配置/还原点/客户机克隆/超管机…）见仓库根 CONTEXT.md。
package control
