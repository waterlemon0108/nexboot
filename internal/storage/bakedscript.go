package storage

import (
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"strings"
)

// Windows 镜像在导入时烘焙开机脚本，下游克隆都继承。镜像是否过期靠版本串比较，
// 所以版本必须覆盖整个注入布局（写哪些文件、叫什么、如何向 gpsvc 注册），而不只是脚本正文。
// 布局以数据形式放在这里并参与哈希，改动后旧镜像会自动失效。
const (
	// MountScriptName 是开机脚本本身，由 control/adapt 生成。
	MountScriptName = "mount-disks.ps1"
	// MountLauncherName 是 gpsvc 调用的 cmd 包装：scripts.ini 只能注册命令，
	// `start` 让 PowerShell 脱离，组策略开机阶段不会因等盘符而卡住登录界面。
	MountLauncherName = "ndmount.cmd"
	MountLauncherBody = "@echo off\r\nstart \"\" powershell -NoProfile -ExecutionPolicy Bypass -File \"%~dp0" + MountScriptName + "\"\r\n"
	// GroupPolicyINI 列出 Scripts 客户端扩展 GUID，gpsvc 才会处理本地策略；Version 取大值以强制重新处理。
	GroupPolicyINI = "[General]\r\ngPCMachineExtensionNames=[{" + ScriptsCSEGUID + "}{40B6664F-4972-11D1-A7CA-0000F87571E3}]\r\nVersion=65539\r\n"
	// ScriptsCSEGUID 用于判断 gpt.ini 是否已注册 Scripts 扩展。
	ScriptsCSEGUID = "42B5FAAE-6536-11D2-AE5A-0000F87571E3"

	// 挂载后 Windows 卷内的路径，用斜杠是因为指的是镜像内位置而非本机。
	// 本地组策略纯靠文件，离线镜像才能注册而不必改注册表 hive。
	groupPolicyPath   = "Windows/System32/GroupPolicy"
	machineScriptPath = groupPolicyPath + "/Machine/Scripts"
	startupPath       = machineScriptPath + "/Startup"
)

// GroupPolicyDir 是挂载卷内存放 gpt.ini 的目录；MachineScriptsDir 放 scripts.ini，StartupScriptDir 放脚本本身。
func GroupPolicyDir(mnt string) string {
	return filepath.Join(mnt, filepath.FromSlash(groupPolicyPath))
}
func MachineScriptsDir(mnt string) string {
	return filepath.Join(mnt, filepath.FromSlash(machineScriptPath))
}
func StartupScriptDir(mnt string) string { return filepath.Join(mnt, filepath.FromSlash(startupPath)) }

// MountScriptLayoutID 标识注入布局（除脚本正文外的一切），并入镜像记录的版本，
// 上述任一项变化都会让旧布局镜像失效。失效镜像不会坏，开机时退回逐台注入。
func MountScriptLayoutID() string {
	sum := sha256.Sum256([]byte(strings.Join([]string{
		startupPath,
		MountScriptName,
		MountLauncherName,
		MountLauncherBody,
		GroupPolicyINI,
	}, "\x00")))
	return hex.EncodeToString(sum[:])
}
