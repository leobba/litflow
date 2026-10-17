# start-litflow.ps1 —— 可靠地启动 LitFlow，并在浏览器打开面板。
#
# 为什么需要它：网关死掉之后面板的每一个 fetch 都会报 "Failed to fetch"，
# 而这句话不提示「服务没在跑」。用户看到的是「配置显示失败」，不是「程序没启动」。
# 这个脚本做三件事：
#   1. 已经在跑就直接开面板（不重复启动，第二个实例会抢不到端口）
#   2. 没在跑就启动，并等到 /health 真的通了才开浏览器
#   3. 失败时把日志尾部打出来，而不是让用户对着一个空白页猜
#
# 用法：双击 start-litflow.cmd，或者 powershell -File start-litflow.ps1

$ErrorActionPreference = 'Stop'

$Root    = 'D:\workbuddy2api'
$Exe     = Join-Path $Root 'wb2api.exe'
$Cfg     = Join-Path $Root 'config.json'
$LogFile = Join-Path $Root 'litflow.log'
$ErrFile = Join-Path $Root 'litflow.err'
$Port    = 7863
$Url     = "http://127.0.0.1:$Port/panel/"

function Test-Listening {
    param([int]$P)
    $c = Get-NetTCPConnection -LocalPort $P -State Listen -ErrorAction SilentlyContinue
    return [bool]$c
}

function Test-Healthy {
    try {
        $r = Invoke-WebRequest "http://127.0.0.1:$Port/health" -UseBasicParsing -TimeoutSec 3 -Proxy $null
        return $r.StatusCode -eq 200
    } catch { return $false }
}

if (-not (Test-Path $Exe)) {
    Write-Host "找不到 $Exe" -ForegroundColor Red
    exit 1
}

if (Test-Listening -P $Port) {
    Write-Host "LitFlow 已在运行（端口 $Port）。" -ForegroundColor Green
} else {
    Write-Host "正在启动 LitFlow…"
    # 输出重定向到文件而不是管道：进程独立持有文件句柄，启动它的 shell 退出后
    # 网关仍能继续写日志 —— 用管道时父进程一退，子进程的下一次写入就会失败。
    Start-Process -FilePath $Exe `
        -ArgumentList '-config', $Cfg `
        -WorkingDirectory $Root `
        -RedirectStandardOutput $LogFile `
        -RedirectStandardError  $ErrFile `
        -WindowStyle Hidden

    # 等真的能受理请求，而不是「进程起来了」。模型目录要预热，实测 2-8 秒。
    $ok = $false
    for ($i = 0; $i -lt 40; $i++) {
        Start-Sleep -Milliseconds 500
        if (Test-Healthy) { $ok = $true; break }
    }
    if (-not $ok) {
        Write-Host "启动失败：40 次探活都没通。" -ForegroundColor Red
        if (Test-Path $ErrFile) {
            Write-Host "`n--- 日志尾部 ---" -ForegroundColor Yellow
            Get-Content $ErrFile -Tail 20 | ForEach-Object { Write-Host "  $_" }
        }
        exit 1
    }
    Write-Host "已启动。" -ForegroundColor Green
}

# 顺便把插件包和配置文件的位置说清楚，省得去文件系统里找
Write-Host ""
Write-Host "  面板      $Url"
Write-Host "  插件包    $(Join-Path $Root 'dualsync@zotero.local.xpi')"
Write-Host "  日志      $ErrFile"
Write-Host ""

Start-Process $Url
