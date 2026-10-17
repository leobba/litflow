# start-litflow.ps1 —— 启动 LitFlow，并在 -Watch 下持续监管它。
#
# 为什么需要监管：网关这一轮静默死过两次，两次都是用户看到面板 "Failed to fetch"
# 才知道。一个要挂在后台的工具，死了不回来就等于没装。
#
# 用法：
#   powershell -File start-litflow.ps1            启动一次（退出即结束）
#   powershell -File start-litflow.ps1 -Watch     启动并监管（.cmd 双击入口走这个）
#   powershell -File start-litflow.ps1 -Stop      停掉监管进程与网关
#
# 监管策略：
#   · 网关退出后等 3 秒重启
#   · 连续 5 次启不来 → 放弃并打印日志尾部，不无限刷屏
#   · 只有第一次成功启动会打开浏览器；重启不打扰用户

param(
    [switch]$Watch,
    [switch]$Stop,
    [int]$Port = 7863
)

$ErrorActionPreference = 'Stop'

$Root    = 'D:\workbuddy2api'
$Exe     = Join-Path $Root 'wb2api.exe'
$Cfg     = Join-Path $Root 'config.json'
$LogFile = Join-Path $Root 'litflow.log'
$ErrFile = Join-Path $Root 'litflow.err'
$SupPid  = Join-Path $Root '.litflow-supervisor.pid'
$GwPid   = Join-Path $Root '.litflow-gateway.pid'
$SupLog  = Join-Path $Root 'litflow-supervisor.log'
$Url     = "http://127.0.0.1:$Port/panel/"

function Write-SupLog([string]$msg) {
    $line = "{0}  {1}" -f (Get-Date -Format 'yyyy-MM-dd HH:mm:ss'), $msg
    Add-Content -Path $SupLog -Value $line -Encoding UTF8
}

function Test-Listening {
    $c = Get-NetTCPConnection -LocalPort $Port -State Listen -ErrorAction SilentlyContinue
    return [bool]$c
}

function Test-Healthy {
    try {
        $r = Invoke-WebRequest "http://127.0.0.1:$Port/health" -UseBasicParsing -TimeoutSec 3 -Proxy $null
        return $r.StatusCode -eq 200
    } catch { return $false }
}

# ── 停止 ────────────────────────────────────────────────────────────────
if ($Stop) {
    foreach ($f in @($SupPid, $GwPid)) {
        if (-not (Test-Path $f)) { continue }
        $pidTxt = (Get-Content $f -Raw).Trim()
        if ($pidTxt -match '^\d+$') {
            $p = Get-Process -Id ([int]$pidTxt) -ErrorAction SilentlyContinue
            if ($p) {
                Write-Host "结束 $($p.ProcessName) (PID $pidTxt)…"
                Stop-Process -Id ([int]$pidTxt) -Force -ErrorAction SilentlyContinue
            }
        }
        Remove-Item $f -Force -ErrorAction SilentlyContinue
    }
    # 监管进程可能因为 Stop 时正在 sleep 而残留，兜底再扫一次
    Get-CimInstance Win32_Process -Filter "Name = 'wb2api.exe'" |
        Where-Object { $_.CommandLine -match 'workbuddy2api' } |
        ForEach-Object { Stop-Process -Id $_.ProcessId -Force -ErrorAction SilentlyContinue }
    Write-Host "已停止。"
    exit 0
}

if (-not (Test-Path $Exe)) {
    Write-Host "找不到 $Exe" -ForegroundColor Red
    exit 1
}

# 已经有监管进程在跑就不重复启动（第二个实例只会抢端口）。
if (Test-Path $SupPid) {
    $old = (Get-Content $SupPid -Raw).Trim()
    if ($old -match '^\d+$' -and (Get-Process -Id ([int]$old) -ErrorAction SilentlyContinue)) {
        Write-Host "LitFlow 监管进程已在运行（PID $old）。面板：$Url" -ForegroundColor Green
        if (-not $Watch) { Start-Process $Url }
        exit 0
    }
    Remove-Item $SupPid -Force -ErrorAction SilentlyContinue
}

# 网关自己已经在监听（比如手动起的），也不必再拉一个。
if (Test-Listening) {
    Write-Host "端口 $Port 已在监听 —— LitFlow 似乎已在运行。" -ForegroundColor Green
    Write-Host "面板：$Url"
    if (-not $Watch) { Start-Process $Url }
    exit 0
}

function Start-Gateway {
    # 输出重定向到文件而不是管道：进程独立持有文件句柄，启动它的 shell 退出后
    # 网关仍能继续写日志。用管道时父进程一退，子进程下一次写入就会失败。
    $p = Start-Process -FilePath $Exe `
        -ArgumentList '-config', $Cfg `
        -WorkingDirectory $Root `
        -RedirectStandardOutput $LogFile `
        -RedirectStandardError  $ErrFile `
        -PassThru -WindowStyle Hidden
    Set-Content -Path $GwPid -Value $p.Id -Encoding ASCII
    return $p
}

# 等真的能受理请求，而不是「进程起来了」。模型目录要预热，实测 2-8 秒。
function Wait-Healthy([int]$tries = 40) {
    for ($i = 0; $i -lt $tries; $i++) {
        Start-Sleep -Milliseconds 500
        if (Test-Healthy) { return $true }
    }
    return $false
}

function Show-Tail {
    if (Test-Path $ErrFile) {
        Write-Host "`n--- 日志尾部 ---" -ForegroundColor Yellow
        Get-Content $ErrFile -Tail 20 | ForEach-Object { Write-Host "  $_" }
    }
}

if (-not $Watch) {
    Write-Host "正在启动 LitFlow…"
    $gw = Start-Gateway
    if (-not (Wait-Healthy)) {
        Write-Host "启动失败：40 次探活都没通。" -ForegroundColor Red
        Show-Tail
        exit 1
    }
    Write-Host "已启动。" -ForegroundColor Green
    Write-Host ""
    Write-Host "  面板    $Url"
    Write-Host "  插件包  $(Join-Path $Root 'dualsync@zotero.local.xpi')"
    Write-Host "  日志    $ErrFile"
    Write-Host ""
    Start-Process $Url
    exit 0
}

# ── 监管模式 ────────────────────────────────────────────────────────────
Set-Content -Path $SupPid -Value $PID -Encoding ASCII
Write-SupLog "supervisor started (pid $PID)"

$openedBrowser = $false
$failStreak = 0
$stopRequested = $false

while (-not $stopRequested) {
    $healthy = $false
    $gw = $null
    try {
        $gw = Start-Gateway
        Write-SupLog "gateway started (pid $($gw.Id))"
        $healthy = Wait-Healthy 40
        if ($healthy) {
            $failStreak = 0
            Write-SupLog "gateway healthy"
            if (-not $openedBrowser) {
                $openedBrowser = $true
                Write-Host "LitFlow 已启动：$Url" -ForegroundColor Green
                Start-Process $Url
            }
        } else {
            $failStreak++
            Write-SupLog "gateway did NOT become healthy (streak $failStreak)"
        }
        # 等它退出。Wait-Process -Timeout 会抛异常，所以轮询。
        while (-not $gw.HasExited) { Start-Sleep -Seconds 2 }
        Write-SupLog "gateway exited with code $($gw.ExitCode)"
    } catch {
        $failStreak++
        Write-SupLog "supervisor error: $($_.Exception.Message)"
    }

    if (-not $healthy -and $failStreak -ge 5) {
        Write-SupLog "giving up after $failStreak failed starts"
        Write-Host ""
        Write-Host "网关连续 5 次启动失败，已放弃重启。" -ForegroundColor Red
        Write-Host "看日志：$ErrFile" -ForegroundColor Yellow
        Show-Tail
        Remove-Item $SupPid -Force -ErrorAction SilentlyContinue
        exit 1
    }

    # 退出后等 3 秒；期间若 pid 文件被删说明有人要求停
    for ($i = 0; $i -lt 3; $i++) {
        Start-Sleep -Seconds 1
        if (-not (Test-Path $SupPid)) { $stopRequested = $true; break }
    }
}

Remove-Item $SupPid -Force -ErrorAction SilentlyContinue
Write-SupLog "supervisor exiting"
