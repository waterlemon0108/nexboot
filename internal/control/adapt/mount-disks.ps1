<#
  ndiskless 客户机启动脚本。由服务端在每次开机时合并进系统克隆盘的「本地 GPO
  机器启动脚本」（随驱动适配注入时同样携带），每台客户机每次开机以 SYSTEM 运行：
    1) 从当前 iSCSI 会话取服务端地址（能无盘开机则必可达）；
    2) GET /boot/net-config?mac= 恢复默认网关与 DNS —— C-1 在 sanhook 前清零了
       网关（防 Windows 对同网段 iSCSI 目标建坏路由），Windows 按 iBFT 把启动
       网卡配成静态且不再 DHCP，不补路由客户机就上不了网。只加路由、不动 IP
       配置，iSCSI 会话不受影响；同网段目标因最长前缀匹配永远直连，另加 /32
       直连路由双保险；
    3) GET /boot/data-disks?mac= 拉取数据盘 LUN→盘符 映射，按 SCSI LUN 定位
       iSCSI 数据盘并校正盘符（幂等）。
  管理台改配置后客户机下次开机即生效。任何失败只写日志、不阻塞启动。
  日志：C:\ndadapt\mount.log（每次开机覆盖，只反映最近一次）。
#>
$ErrorActionPreference = 'Continue'
$port = '__ND_API_PORT__'
$work = 'C:\ndadapt'
New-Item -ItemType Directory -Force -Path $work | Out-Null
$logFile = Join-Path $work 'mount.log'
"start $(Get-Date -Format o) as $(whoami)" | Out-File $logFile -Encoding utf8
function Log($m) { $m | Out-File $logFile -Append -Encoding utf8 }

# 数据盘 = 系统 iSCSI target 上 LUN>=1 的盘；返回其 Win32_DiskDrive（含 Index/SCSILogicalUnit）
function Find-DataDisk($lun) {
  $iscsiNums = @(Get-Disk | Where-Object { $_.BusType -eq 'iSCSI' } | ForEach-Object Number)
  Get-CimInstance Win32_DiskDrive | Where-Object {
    [int]$_.SCSILogicalUnit -eq [int]$lun -and $iscsiNums -contains [int]$_.Index
  } | Select-Object -First 1
}

# 盘上最大的普通数据分区（数据盘通常单分区；跳过恢复/保留分区）
function Get-DataPartition($diskIndex) {
  @(Get-Partition -DiskNumber $diskIndex -ErrorAction SilentlyContinue |
    Where-Object { $_.Type -in @('Basic', 'IFS', 'Logical', 'Huge') }) |
    Sort-Object Size -Descending | Select-Object -First 1
}

try {
  # 1) 服务端地址与本端地址：都取自当前 iSCSI 会话
  $sessions = @(Get-CimInstance -Namespace root/wmi -ClassName MSiSCSIInitiator_SessionClass -ErrorAction SilentlyContinue |
    ForEach-Object { $_.ConnectionInformation })
  $addrs = @($sessions | ForEach-Object { $_.TargetAddress } | Where-Object { $_ } | Sort-Object -Unique)
  $initAddrs = @($sessions | ForEach-Object { $_.InitiatorAddress } | Where-Object { $_ } | Sort-Object -Unique)
  if (-not $addrs.Count) { Log 'no iscsi session address; skip'; exit 0 }

  # 逐物理网卡 MAC 查询（服务端按启动网卡 MAC 记录终端，试到命中为止）
  $macs = @(Get-CimInstance Win32_NetworkAdapter |
    Where-Object { $_.PhysicalAdapter -and $_.MACAddress } |
    ForEach-Object { $_.MACAddress } | Sort-Object -Unique)
  $net = $null
  $items = $null
  foreach ($addr in $addrs) {
    foreach ($mac in $macs) {
      try {
        $net = Invoke-RestMethod -Uri "http://${addr}:${port}/boot/net-config?mac=${mac}" -TimeoutSec 5
        try {
          $resp = Invoke-RestMethod -Uri "http://${addr}:${port}/boot/data-disks?mac=${mac}" -TimeoutSec 5
          $items = @($resp.items)
        } catch { Log "data-disks fetch failed: $($_.Exception.Message)" }
        Log "config from ${addr} mac=${mac}: net=$($net | ConvertTo-Json -Compress) disks=$($items | ConvertTo-Json -Compress)"
        break
      } catch { }
    }
    if ($null -ne $net) { break }
  }
  if ($null -eq $net) { Log 'net-config api unreachable; skip all' ; exit 0 }

  # 2) 恢复网关/DNS（先于数据盘处理，让联网尽快就绪）
  $gw = "$($net.gateway)".Trim()
  if ($gw) {
    # 启动网卡 = 持有 iSCSI 会话本端地址的网卡
    $ifIndex = $null
    foreach ($ia in $initAddrs) {
      $ip = Get-NetIPAddress -AddressFamily IPv4 -IPAddress $ia -ErrorAction SilentlyContinue | Select-Object -First 1
      if ($ip) { $ifIndex = $ip.InterfaceIndex; break }
    }
    if ($null -eq $ifIndex) {
      Log "gateway: boot NIC not found (initiator addrs: $($initAddrs -join ','))"
    } else {
      # 双保险：把 iSCSI 服务器钉成 /32 直连路由（最长前缀匹配本就直连，此为显式保障）
      foreach ($sa in $addrs) {
        if (-not (Get-NetRoute -DestinationPrefix "$sa/32" -InterfaceIndex $ifIndex -ErrorAction SilentlyContinue)) {
          try {
            New-NetRoute -DestinationPrefix "$sa/32" -InterfaceIndex $ifIndex -ErrorAction Stop | Out-Null
            Log "route: pinned $sa/32 on-link"
          } catch { Log "route: pin ${sa}: $($_.Exception.Message)" }
        }
      }
      $def = Get-NetRoute -DestinationPrefix '0.0.0.0/0' -InterfaceIndex $ifIndex -ErrorAction SilentlyContinue | Select-Object -First 1
      if ($def -and $def.NextHop -eq $gw) {
        Log "gateway: already $gw"
      } else {
        if ($def) {
          try { $def | Remove-NetRoute -Confirm:$false -ErrorAction Stop }
          catch { Log "gateway: remove stale default: $($_.Exception.Message)" }
        }
        try {
          New-NetRoute -DestinationPrefix '0.0.0.0/0' -InterfaceIndex $ifIndex -NextHop $gw -ErrorAction Stop | Out-Null
          Log "gateway: set $gw"
        } catch { Log "gateway: set ${gw}: $($_.Exception.Message)" }
      }
      $dns = @(@($net.dns) | ForEach-Object { "$_".Trim() } | Where-Object { $_ })
      if ($dns.Count) {
        try {
          Set-DnsClientServerAddress -InterfaceIndex $ifIndex -ServerAddresses $dns -ErrorAction Stop
          Log "dns: set $($dns -join ',')"
        } catch { Log "dns: $($_.Exception.Message)" }
      }
    }
  } else { Log 'no gateway configured; skip net restore' }

  # 3) 数据盘盘符校正
  if ($null -eq $items) { Log 'data-disks api unreachable; skip disks'; exit 0 }

  # 目标映射 lun -> 盘符（服务端已校验，这里再兜底过滤一次）
  $targets = @{}
  foreach ($it in $items) {
    $letter = ("$($it.letter)").Trim().TrimEnd(':').ToUpper()
    if ($letter -match '^[D-Z]$') { $targets[[int]$it.lun] = $letter }
    else { Log "skip lun $($it.lun): bad letter '$($it.letter)'" }
  }
  if (-not $targets.Count) { Log 'no data disks configured; done'; exit 0 }

  # 等数据盘 LUN 出现（数据盘卷可能比系统盘晚就绪几秒）
  $found = @{}
  for ($i = 0; $i -lt 30 -and $found.Count -lt $targets.Count; $i++) {
    foreach ($lun in @($targets.Keys)) {
      if (-not $found.ContainsKey($lun)) {
        $d = Find-DataDisk $lun
        if ($d) { $found[$lun] = [int]$d.Index }
      }
    }
    if ($found.Count -lt $targets.Count) { Start-Sleep -Seconds 1 }
  }
  foreach ($lun in @($targets.Keys)) {
    if (-not $found.ContainsKey($lun)) { Log "lun ${lun}: disk not found" }
  }

  # 数据盘可能被 SAN policy 置为离线/只读，先拉上线
  foreach ($lun in @($found.Keys)) {
    $gd = Get-Disk -Number $found[$lun] -ErrorAction SilentlyContinue
    if ($gd -and $gd.IsOffline) { Set-Disk -Number $gd.Number -IsOffline $false }
    if ($gd -and $gd.IsReadOnly) { Set-Disk -Number $gd.Number -IsReadOnly $false }
  }

  # 两遍设置：先释放我们管理的、盘符不匹配的分区（避免 D↔E 互换互相占位），再赋值
  foreach ($lun in @($found.Keys)) {
    $part = Get-DataPartition $found[$lun]
    if ($part -and $part.DriveLetter -and $part.DriveLetter -ne $targets[$lun]) {
      try { $part | Remove-PartitionAccessPath -AccessPath "$($part.DriveLetter):\" }
      catch { Log "lun ${lun}: free $($part.DriveLetter): failed: $($_.Exception.Message)" }
    }
  }
  foreach ($lun in @($found.Keys | Sort-Object)) {
    $target = $targets[$lun]
    $part = Get-DataPartition $found[$lun]
    if (-not $part) { Log "lun ${lun}: no data partition"; continue }
    if ($part.DriveLetter -eq $target) { Log "lun ${lun}: already ${target}:"; continue }
    try {
      $part | Set-Partition -NewDriveLetter $target
      Log "lun ${lun}: set ${target}:"
    } catch { Log "lun ${lun}: set ${target}: failed: $($_.Exception.Message)" }
  }
} catch {
  Log "exception: $($_.Exception.Message)"
}
Log "done $(Get-Date -Format o)"
