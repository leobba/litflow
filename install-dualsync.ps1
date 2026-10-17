# install-dualsync.ps1 —— 把 Dual Sync 直接放进 Zotero 的 extensions 目录。
#
# 为什么需要这个：Zotero 的「Install Add-on From File」会校验 manifest 里的
# update_url 等字段，不通过时**静默不安装**（extensions.json 里连一条记录都没有，
# 界面上也看不到任何菜单）。这台机器上你已有的 6 个插件全都是 extensions/<id>.xpi
# 这种形态——直接放文件是绕开校验的可靠路径，装出来的结果与 UI 安装完全一致。
#
# 用法：完全退出 Zotero → 运行本脚本 → 重新打开 Zotero。

$ErrorActionPreference = 'Stop'

$src = "D:\workbuddy2api\dualsync@zotero.local.xpi"
if (-not (Test-Path $src)) { $src = "D:\zotero-dual-sync\dualsync@zotero.local.xpi" }
if (-not (Test-Path $src)) { Write-Host "找不到 xpi：先去面板 http://127.0.0.1:7863/panel/ 点『下载插件』"; exit 1 }

# Zotero 必须在关闭状态：运行中它会重写 extensions.json，写入被丢弃。
$proc = Get-Process zotero -ErrorAction SilentlyContinue
if ($proc) {
    Write-Host "Zotero 正在运行（PID $($proc.Id)）。"
    Write-Host "请先完全退出 Zotero（任务栏图标也要退出），再运行本脚本。"
    exit 1
}

$profiles = @()
$ini = Join-Path $env:APPDATA "Zotero\Zotero\profiles.ini"
if (Test-Path $ini) {
    foreach ($line in Get-Content $ini) {
        if ($line -match '^Path=(.+)$') {
            $rel = $Matches[1].Trim()
            $profiles += (Join-Path (Join-Path $env:APPDATA "Zotero\Zotero") $rel)
        }
    }
}
if ($profiles.Count -eq 0) {
    $profiles = Get-ChildItem (Join-Path $env:APPDATA "Zotero\Zotero\Profiles") -Directory |
                Where-Object { Test-Path (Join-Path $_.FullName "prefs.js") } |
                ForEach-Object { $_.FullName }
}
if ($profiles.Count -eq 0) { Write-Host "找不到 Zotero profile"; exit 1 }

foreach ($prof in $profiles) {
    $ext = Join-Path $prof "extensions"
    New-Item -ItemType Directory -Force -Path $ext | Out-Null
    $dst = Join-Path $ext "dualsync@zotero.local.xpi"
    Copy-Item $src $dst -Force
    $size = [math]::Round((Get-Item $dst).Length / 1KB, 1)
    Write-Host "  已放置 $dst  ($size KB)"
    Write-Host "  profile: $prof"
}

Write-Host ""
Write-Host "完成。现在重新打开 Zotero，然后看："
Write-Host "  工具菜单 → 应该出现 5 项『Dual Sync：…』"
Write-Host "  某一篇文献上右键 → 应该出现『Dual Sync：这篇不翻译 / 恢复翻译』"
Write-Host ""
Write-Host "翻译入口就是：工具 → Dual Sync：翻译选中条目"
Write-Host "（先在文献列表里选中要翻的条目，再点它）"
