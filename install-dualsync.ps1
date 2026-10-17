# install-dualsync.ps1 —— 把 Dual Sync 直接放进 Zotero 的 extensions 目录。
#
# 为什么需要它：Zotero 的「Install Add-on From File」在拒绝一个包时只弹一句
# 「可能无法与该版本的 Zotero 兼容」，不说原因。而这个 profile 里能用的 6 个
# 插件全都是 extensions/<id>.xpi 这种形态 —— 直接放文件就是同一个终态，
# 只是不经过安装器那道校验。
#
# 用法：完全退出 Zotero（任务栏图标也要退）→ 运行本脚本 → 重新打开 Zotero。

$ErrorActionPreference = 'Stop'

$src = "D:\workbuddy2api\dualsync@zotero.local.xpi"
if (-not (Test-Path $src)) { $src = "D:\zotero-dual-sync\dualsync@zotero.local.xpi" }
if (-not (Test-Path $src)) {
    Write-Host "找不到 xpi。先去面板 http://127.0.0.1:7863/panel/ 点『下载插件』"
    exit 1
}

# Zotero 运行时会重写 extensions.json，此刻写入会被它覆盖掉，所以先要求退出。
$proc = Get-Process zotero -ErrorAction SilentlyContinue
if ($proc) {
    Write-Host "Zotero 正在运行（PID $($proc.Id)）。"
    Write-Host "请先完全退出 Zotero，再运行本脚本。"
    exit 1
}

$profiles = @()
$ini = Join-Path $env:APPDATA "Zotero\Zotero\profiles.ini"
if (Test-Path $ini) {
    foreach ($line in Get-Content $ini) {
        if ($line -match '^Path=(.+)$') {
            $profiles += (Join-Path (Join-Path $env:APPDATA "Zotero\Zotero") $Matches[1].Trim())
        }
    }
}
if ($profiles.Count -eq 0) {
    $base = Join-Path $env:APPDATA "Zotero\Zotero\Profiles"
    if (Test-Path $base) {
        $profiles = Get-ChildItem $base -Directory |
            Where-Object { Test-Path (Join-Path $_.FullName "prefs.js") } |
            ForEach-Object { $_.FullName }
    }
}
if ($profiles.Count -eq 0) { Write-Host "找不到 Zotero profile"; exit 1 }

foreach ($prof in $profiles) {
    $ext = Join-Path $prof "extensions"
    New-Item -ItemType Directory -Force -Path $ext | Out-Null
    $dst = Join-Path $ext "dualsync@zotero.local.xpi"
    Copy-Item $src $dst -Force
    Write-Host ("  已放置 {0}  ({1} KB)" -f $dst, [math]::Round((Get-Item $dst).Length / 1KB, 1))
    Write-Host ("  profile: {0}" -f $prof)
}

Write-Host ""
Write-Host "完成。重新打开 Zotero 后检查："
Write-Host "  工具菜单 -> 应出现 5 项『Dual Sync：…』"
Write-Host "  文献上右键 -> 应出现『Dual Sync：这篇不翻译 / 恢复翻译』"
Write-Host ""
Write-Host "翻译入口：先在文献列表里选中条目，再点 工具 -> Dual Sync：翻译选中条目"