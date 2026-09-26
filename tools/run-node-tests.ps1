# 本地跑 pvn-node 包测试的包装脚本。
#
# 背景：app/agent/cmd/pvn-node/rsrc_windows_amd64.syso 内嵌 requireAdministrator
# 清单（wintun 建虚拟网卡需要管理员权限，双击启动自动弹 UAC）。go test 生成的
# 测试副本会连这个资源一起链进去，于是在非提权终端里直接报
#   fork/exec ...pvn-node.test.exe: The requested operation requires elevation.
# 这不是测试失败，而是副本被清单要求提权。
#
# 做法与 .github/workflows/stability.yml 的「节点完整回归（测试副本不携带管理员清单）」
# 一致：临时把资源文件移开，跑完在 finally 里还原（异常中断也不会丢）。
#
# 用法：
#   powershell -File tools/run-node-tests.ps1                    # 默认跑 pvn-node 包全部用例
#   powershell -File tools/run-node-tests.ps1 -TestArgs "-count=1,-v,-run,Pending,./..."
#       ↑ go test 参数整体作为一个逗号分隔字符串传入。两条 PowerShell 限制：
#         1) 直接写 -v 会被 PowerShell 当成自己的 -Verbose 吃掉；
#         2) 把数组写成 -count=1,-v,... 会被外层解析器当成参数列表而报错。
param(
    [string[]]$TestArgs,
    [Parameter(ValueFromRemainingArguments = $true)][string[]]$Rest
)

$ErrorActionPreference = 'Stop'
$root = Split-Path -Parent $PSScriptRoot
$resource = Join-Path $root 'app/agent/cmd/pvn-node/rsrc_windows_amd64.syso'
$backup = "$resource.test-backup"
$moved = $false

$goArgs = @('-count=1', './app/agent/cmd/pvn-node/')
$raw = if ($TestArgs) { $TestArgs } elseif ($Rest) { $Rest } else { @() }
if ($raw.Count -gt 0) { $goArgs = ($raw -join ',') -split ',' }

if (Test-Path $resource) {
    Move-Item $resource $backup -Force
    $moved = $true
}

try {
    Push-Location $root
    & go test @goArgs
    $code = $LASTEXITCODE
} finally {
    Pop-Location
    if ($moved) { Move-Item $backup $resource -Force }
}

exit $code
