# 引导升级：给「自更新逻辑本身是坏的」老版本（Windows 0.5.77 及更早）换一次程序。
#
# 为什么需要它：0.5.72~0.5.77 用 MoveFileEx(REPLACE_EXISTING) 覆盖正在运行的 exe，
# 实测一律 Access is denied（见 README「GitHub 在线更新」），所以这些版本点更新必然
# 失败。0.5.78 起改成「改名腾位 + 写入」，但坏掉的那一版没有能力替换自己，只能先
# 用本脚本手工换一次；换完之后 /api/update/apply 就能正常工作了。
#
# 做的事（与 0.5.78+ 的替换语义一致，全程可回滚）：
#   1. 从发行 zip 里取 lanet.exe，按包内 manifest.json 的 sha256 校验
#   2. 复制到 <安装目录>\lanet.exe.swap
#   3. 旧程序改名为 lanet.exe.old-<版本>-<时间戳>（运行中的映像允许改名）
#   4. lanet.exe.swap 就位为 lanet.exe
#   5. 可选 -Restart：调用控制台 POST /api/restart，让服务/进程用新程序起来
#
# 用法：
#   powershell -File tools/lanet-bootstrap-upgrade.ps1 -ReleaseZip .\lanet-0.5.80-windows-amd64.zip -InstallDir D:\lanet-node -Restart
#   powershell -File tools/lanet-bootstrap-upgrade.ps1 -ReleaseZip ... -InstallDir ... -DryRun
#
# 回滚：把 lanet.exe.old-* 改回 lanet.exe（先停服务/进程），或用控制台「更新」重来。
param(
    [Parameter(Mandatory = $true)][string]$ReleaseZip,
    [Parameter(Mandatory = $true)][string]$InstallDir,
    [string]$ConsoleUrl = 'http://127.0.0.1:8900',
    [string]$ExeName = 'lanet.exe',
    [switch]$Restart,
    [switch]$DryRun
)

$ErrorActionPreference = 'Stop'
Add-Type -AssemblyName System.IO.Compression.FileSystem

$zipPath = (Resolve-Path -LiteralPath $ReleaseZip).Path
$exe = Join-Path $InstallDir $ExeName
if (-not (Test-Path -LiteralPath $exe)) { throw "安装目录里没有 $ExeName：$InstallDir" }

$stage = Join-Path $env:TEMP ("lanet-bootstrap-" + [guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Force $stage | Out-Null
try {
    [System.IO.Compression.ZipFile]::ExtractToDirectory($zipPath, $stage)
    $newExe = Get-ChildItem -LiteralPath $stage -Recurse -Filter $ExeName | Select-Object -First 1
    if (-not $newExe) { throw "发行包里没找到 $ExeName" }
    $manifestPath = Get-ChildItem -LiteralPath $stage -Recurse -Filter 'manifest.json' | Select-Object -First 1
    if (-not $manifestPath) { throw '发行包里没有 manifest.json，无法校验' }
    $manifest = Get-Content -LiteralPath $manifestPath.FullName -Raw | ConvertFrom-Json

    $sum = (Get-FileHash -LiteralPath $newExe.FullName -Algorithm SHA256).Hash.ToLower()
    "发行包版本 = $($manifest.version)  平台 = $($manifest.platform)"
    "包内 exe   = $($newExe.Length) 字节  sha256=$sum"
    if ($manifest.sha256 -and $manifest.sha256.ToLower() -ne $sum) {
        throw "包内 exe 摘要与 manifest.json 不一致（期望 $($manifest.sha256)）"
    }
    '包内摘要校验通过 ✓'

    $oldVer = 'unknown'
    try {
        $oldVer = (Invoke-RestMethod -Uri "$ConsoleUrl/api/local-info" -TimeoutSec 8).version
    } catch {
        $live = Join-Path $InstallDir 'VERSION'
        if (Test-Path -LiteralPath $live) { $oldVer = (Get-Content -LiteralPath $live -Raw).Trim() }
    }
    $backup = Join-Path $InstallDir ("$ExeName.old-$oldVer-" + (Get-Date -Format 'yyyyMMdd-HHmmss'))
    "当前运行版本 = $oldVer"
    "将执行：$exe → $backup，再把新版放到 $exe"
    if ($DryRun) { '（DryRun：未改动任何文件）'; exit 0 }

    $swap = "$exe.swap"
    Copy-Item -LiteralPath $newExe.FullName -Destination $swap -Force
    Rename-Item -LiteralPath $exe -NewName (Split-Path $backup -Leaf)
    try {
        Rename-Item -LiteralPath $swap -NewName $ExeName
    } catch {
        Rename-Item -LiteralPath $backup -NewName $ExeName   # 就位失败：立刻改回来
        throw "新版就位失败，已回滚旧程序：$_"
    }
    "新版已就位：$exe（sha256=$((Get-FileHash -LiteralPath $exe -Algorithm SHA256).Hash.ToLower())）"
    "旧程序备份：$backup（需要回滚时把它改回 $ExeName）"

    if ($Restart) {
        '调用控制台重启…'
        $r = Invoke-RestMethod -Method Post -Uri "$ConsoleUrl/api/restart" -TimeoutSec 15
        Start-Sleep -Seconds 25
        try {
            $info = Invoke-RestMethod -Uri "$ConsoleUrl/api/local-info" -TimeoutSec 10
            "重启后运行版本 = $($info.version)  虚拟 IP = $($info.virtual_ip)"
        } catch {
            "重启后控制台无响应，请检查服务状态与 lanet.log：$_"
        }
    } else {
        "未加 -Restart：请手工重启服务/进程（sc stop Lanet; sc start Lanet 或 POST $ConsoleUrl/api/restart）"
    }
} finally {
    Remove-Item -LiteralPath $stage -Recurse -Force -ErrorAction SilentlyContinue
}
