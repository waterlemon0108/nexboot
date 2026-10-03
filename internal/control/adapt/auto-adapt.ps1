<#
  ndiskless 启动驱动安装脚本。由服务端离线注入为「本地 GPO 机器启动脚本」，
  超管机开机时以 SYSTEM 自动运行一次：
    1) 解压 C:\ndadapt\bundle.zip 到 C:\ndadapt\drivers；
    2) PnPUtil 预装全部 INF；
    3) 网卡驱动服务 + iSCSI 栈(msiscsi/iScsiPrt) 注册 boot-start(Start=0)，
       写 HWID→服务 到 CriticalDeviceDatabase(CDDB)，解决换网卡 0x7B；
    4) 写安装状态 C:\ndadapt\done.txt + adapt.log（供排查），自删 GPO 注册。
  装完**不自动关机**：交给用户在客机内确认驱动装上后自己关机，再到管理台
  「停机存还原点」把改动固化成新还原点。
#>
$ErrorActionPreference = 'Continue'
$work = 'C:\ndadapt'
$done = Join-Path $work 'done.txt'
$exit = 0
function Log($m) { $m | Out-File (Join-Path $work 'adapt.log') -Append -Encoding utf8 }

New-Item -ItemType Directory -Force -Path $work | Out-Null
Log ("start " + (Get-Date -Format o) + " as " + (whoami))

try {
  # 1) 解压驱动集
  $drivers = Join-Path $work 'drivers'
  Remove-Item -Recurse -Force $drivers -ErrorAction SilentlyContinue
  New-Item -ItemType Directory -Force -Path $drivers | Out-Null
  $zip = Join-Path $work 'bundle.zip'
  if (Test-Path $zip) {
    Expand-Archive -Path $zip -DestinationPath $drivers -Force
  }
  $infs = Get-ChildItem -Path $drivers -Recurse -Filter *.inf -ErrorAction SilentlyContinue
  Log ("inf count = " + $infs.Count)

  # 2) PnPUtil 逐个预装（BundleArchive 是每包一子目录，逐 inf 安装最稳）
  foreach ($inf in $infs) {
    & pnputil.exe /add-driver $inf.FullName /install 2>&1 | Out-File (Join-Path $work 'pnputil.log') -Append -Encoding utf8
    if ($LASTEXITCODE -ne 0 -and $LASTEXITCODE -ne 259) { $exit = 1; Log "pnputil $($inf.Name) returned $LASTEXITCODE" }
  }

  # 3) iSCSI 栈 boot-start
  foreach ($svc in @('msiscsi', 'iScsiPrt')) {
    $key = "HKLM:\SYSTEM\CurrentControlSet\Services\$svc"
    if (Test-Path $key) { Set-ItemProperty -Path $key -Name Start -Value 0 -Type DWord }
    elseif ($svc -eq 'msiscsi') { $exit = 2; Log "msiscsi service missing" }
  }
  # 网卡驱动服务 boot-start + CDDB
  foreach ($inf in $infs) {
    $svcNames = Select-String -Path $inf.FullName -Pattern 'AddService\s*=\s*([^,\s]+)' -AllMatches |
      ForEach-Object { $_.Matches } | ForEach-Object { $_.Groups[1].Value } | Sort-Object -Unique
    $hwids = Select-String -Path $inf.FullName -Pattern '(PCI\\VEN_[0-9A-Fa-f]{4}&DEV_[0-9A-Fa-f]{4}[^,\s"]*)' -AllMatches |
      ForEach-Object { $_.Matches } | ForEach-Object { $_.Groups[1].Value } | Sort-Object -Unique
    foreach ($svc in $svcNames) {
      $key = "HKLM:\SYSTEM\CurrentControlSet\Services\$svc"
      if (Test-Path $key) {
        Set-ItemProperty -Path $key -Name Start -Value 0 -Type DWord
        foreach ($hwid in $hwids) {
          $cddbId = ($hwid -replace '\\', '#').ToUpper()
          if ($cddbId -match '^(PCI#VEN_[0-9A-F]{4}&DEV_[0-9A-F]{4})') {
            $cddbKey = "HKLM:\SYSTEM\CurrentControlSet\Control\CriticalDeviceDatabase\$($Matches[1])"
            if (-not (Test-Path $cddbKey)) { New-Item -Path $cddbKey -Force | Out-Null }
            Set-ItemProperty -Path $cddbKey -Name Service -Value $svc -Type String
            Set-ItemProperty -Path $cddbKey -Name ClassGUID -Value '{4d36e972-e325-11ce-bfc1-08002be10318}' -Type String
          }
        }
      } elseif ($exit -eq 0) { $exit = 1; Log "service $svc not registered" }
    }
  }
} catch {
  $exit = 3; Log ("exception: " + $_.Exception.Message)
}

# 4) 安装状态（供用户/管理台排查，非自动判定信号）
"OK=$($exit -eq 0) EXIT=$exit AT=$(Get-Date -Format o)" | Out-File $done -Encoding utf8

# 5) 摘掉自己的启动脚本注册：只装一次，否则会随「存还原点」固化进镜像，以后每次开机都重跑。
#    只删 ndadapt.cmd 这一项，镜像自带的组策略和 ndmount 原样保留；改完递增 gpt.ini 版本让 gpsvc 重新处理。
function Remove-AdaptRegistration([string]$gp) {
  $ini = Join-Path $gp 'Machine\Scripts\scripts.ini'
  if (Test-Path $ini) {
    $bytes = [IO.File]::ReadAllBytes($ini)
    $utf16 = $bytes.Length -ge 2 -and $bytes[0] -eq 0xFF -and $bytes[1] -eq 0xFE
    $enc = if ($utf16) { New-Object Text.UnicodeEncoding($false, $true) } else { [Text.Encoding]::Default }
    $text = $enc.GetString($bytes).TrimStart([char]0xFEFF)
    $out = New-Object System.Collections.Generic.List[string]
    $items = New-Object System.Collections.Generic.List[object]
    $inStartup = $false
    foreach ($line in ($text -split "`r?`n")) {
      if ($line -match '^\s*\[(.+)\]\s*$') {
        if ($inStartup) { Add-StartupItems $out $items }
        $inStartup = ($Matches[1] -ieq 'Startup')
        $out.Add($line); continue
      }
      if ($inStartup -and $line -match '^\s*(\d+)(CmdLine|Parameters)=(.*)$') {
        $n = [int]$Matches[1]
        $item = $items | Where-Object { $_.N -eq $n } | Select-Object -First 1
        if (-not $item) { $item = [pscustomobject]@{ N = $n; CmdLine = ''; Parameters = '' }; $items.Add($item) }
        $item.($Matches[2]) = $Matches[3]
        continue
      }
      if ($inStartup -and $line.Trim() -eq '') { continue } # 段内空行会把条目挤到它后面
      $out.Add($line)
    }
    if ($inStartup) { Add-StartupItems $out $items }
    [IO.File]::WriteAllText($ini, (($out -join "`r`n").TrimEnd() + "`r`n"), $enc)
  }
  $startup = Join-Path $gp 'Machine\Scripts\Startup'
  Remove-Item -Force (Join-Path $startup 'adapt.ps1'), (Join-Path $startup 'ndadapt.cmd') -ErrorAction SilentlyContinue
  $gpt = Join-Path $gp 'gpt.ini'
  if (Test-Path $gpt) {
    $g = [IO.File]::ReadAllText($gpt)
    $g = [regex]::Replace($g, '(?m)^Version=(\d+)', { param($m) 'Version=' + ([int64]$m.Groups[1].Value + 1) })
    [IO.File]::WriteAllText($gpt, $g, [Text.Encoding]::Default)
  }
}

# Add-StartupItems 写回 [Startup] 段里除 ndadapt.cmd 外的条目，序号从 0 重排。
function Add-StartupItems($out, $items) {
  $k = 0
  foreach ($it in ($items | Sort-Object N)) {
    if ($it.CmdLine -ieq 'ndadapt.cmd') { continue }
    $out.Add("$($k)CmdLine=$($it.CmdLine)"); $out.Add("$($k)Parameters=$($it.Parameters)"); $k++
  }
  $items.Clear()
}

$gp = 'C:\Windows\System32\GroupPolicy'
try {
  Remove-AdaptRegistration $gp
} catch {
  # 合并失败时退回旧做法：宁可丢掉镜像自带策略，也不能让适配脚本每次开机都重跑。
  Log "remove adapt registration failed, falling back: $_"
  $startup = Join-Path $gp 'Machine\Scripts\Startup'
  if (Test-Path (Join-Path $startup 'ndmount.cmd')) {
    "[Startup]`r`n0CmdLine=ndmount.cmd`r`n0Parameters=" |
      Out-File (Join-Path $gp 'Machine\Scripts\scripts.ini') -Encoding ascii
    Remove-Item -Force (Join-Path $startup 'adapt.ps1'), (Join-Path $startup 'ndadapt.cmd') -ErrorAction SilentlyContinue
  } else {
    Remove-Item -Recurse -Force $gp -ErrorAction SilentlyContinue
  }
}

# 不自动关机：驱动已安装，留给用户在客机内确认后自己关机、再「停机存还原点」。
Log "install done exit=$exit; drivers installed, awaiting user shutdown + save reduction"
