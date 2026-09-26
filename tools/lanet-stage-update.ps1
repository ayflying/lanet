# 手工把候选程序按 staged 自更新的落盘格式放进安装目录，用于验证「暂存 → 重启切换」链路。
#
# 产出的就是 stageNewBinary 会写的那两个文件（见 app/agent/cmd/pvn-node/update.go）：
#   <安装目录>\lanet.exe.pending        候选程序（原样复制）
#   <安装目录>\lanet.exe.pending.json   {"sha256":"<候选程序 sha256>"}
# 之后调用 POST /api/restart（或 /api/update/apply）即可让节点在重启时切换：
#   普通模式 → 更新辅助进程（-update-helper）
#   Windows 服务模式 → -service-restart 辅助进程，SCM stop → 替换 → start
#
# 典型用途：手上没有「更高的已发布版本」时，用本地构建的程序验证切换与回滚。
#
# 用法：
#   powershell -File tools/lanet-stage-update.ps1 -Candidate .\lanet-new.exe -InstallDir D:\lanet-node
#   powershell -File tools/lanet-stage-update.ps1 -Candidate ... -InstallDir ... -DryRun
param(
    [Parameter(Mandatory = $true)][string]$Candidate,
    [Parameter(Mandatory = $true)][string]$InstallDir,
    [string]$ExeName = 'lanet.exe',
    [switch]$DryRun
)

$ErrorActionPreference = 'Stop'
$candidate = (Resolve-Path -LiteralPath $Candidate).Path
$exe = Join-Path $InstallDir $ExeName
if (-not (Test-Path -LiteralPath $exe)) {
    throw "安装目录里没有 $ExeName：$InstallDir"
}
if ((Get-Item -LiteralPath $candidate).Length -eq 0) {
    throw "候选程序是空文件：$candidate"
}

$pending = "$exe.pending"
$marker = "$exe.pending.json"
if ((Test-Path -LiteralPath $marker) -and -not $DryRun) {
    throw "已存在待更新标记 $marker（先重启节点消费它，或手工删掉再试）"
}

$sum = (Get-FileHash -LiteralPath $candidate -Algorithm SHA256).Hash.ToLower()
"候选：$candidate"
"  大小   = $((Get-Item -LiteralPath $candidate).Length)"
"  sha256 = $sum"
"目标：$pending"
"标记：$marker"

if ($DryRun) {
    '（DryRun：未写入任何文件）'
    exit 0
}

Copy-Item -LiteralPath $candidate -Destination $pending -Force
# 标记写成原子落位（.part → rename），与程序内 writeAtomicBytes 一致。
$json = '{"sha256":"' + $sum + '"}'
Set-Content -LiteralPath "$marker.part" -Value $json -NoNewline -Encoding ascii
Move-Item -LiteralPath "$marker.part" -Destination $marker -Force
'已暂存。调用 POST /api/restart 让节点在重启时切换（普通模式走更新辅助进程，服务模式走 SCM stop/start）。'
