# package.ps1 —— 组装可分发目录与 zip。
#
# 目标不是「再压一次 exe」（CI 已经在做），而是**把非二进制资产一起带上**：
# 插件 xpi、启动/停止脚本、配置模板、快速开始说明。少了这些，用户拿到的只是一个
# 不知道该怎么用的 exe —— 而这一轮的绝大多数支持成本都花在「先装什么、怎么配」上。
#
# 用法：powershell -File scripts/package.ps1 [-Version v1.13.0] [-SkipBuild]

param(
    [string]$Version = '',
    [switch]$SkipBuild
)

$ErrorActionPreference = 'Stop'
$Root = Split-Path -Parent (Split-Path -Parent $MyInvocation.MyCommand.Path)
Set-Location $Root

$py = 'D:\Program Files\Python\python.exe'
if (-not (Test-Path $py)) { $py = 'python' }

# 版本号只有一个来源：cmd/server/main.go 的 appVersion
if (-not $Version) {
    $hit = Select-String -Path 'cmd/server/main.go' -Pattern 'appVersion = "([^"]+)"' | Select-Object -First 1
    if (-not $hit) { throw '找不到 appVersion' }
    $Version = 'v' + ($hit.Matches[0].Groups[1].Value -replace '-panel$', '')
}
$verNum = $Version.TrimStart('v')
Write-Host "打包 $Version"

$dist = Join-Path $Root 'dist'
$pkg  = Join-Path $dist "litflow-$verNum-windows-x64"
if (Test-Path $pkg) { Remove-Item $pkg -Recurse -Force }
New-Item -ItemType Directory -Force -Path $pkg | Out-Null

# ── 1. 编译网关 ────────────────────────────────────────────────────────
$exe = Join-Path $pkg 'wb2api.exe'
if (-not $SkipBuild) {
    Write-Host '  编译 wb2api.exe …'
    $go = 'D:\go-toolchain\go\bin\go.exe'
    if (-not (Test-Path $go)) { $go = 'go' }
    $env:GOOS = 'windows'; $env:GOARCH = 'amd64'; $env:CGO_ENABLED = '0'
    # -s -w 剥离调试信息：体积从 ~14MB 降到 ~9MB。这个仓库的发布纪律要求这么做。
    & $go build -trimpath -ldflags '-s -w' -o $exe ./cmd/server
    if ($LASTEXITCODE -ne 0) { throw "go build 失败（exit $LASTEXITCODE）" }
} else {
    Copy-Item (Join-Path $Root 'wb2api.exe') $exe -Force
}

# ── 2. 构建插件 xpi（与网关同一次发版，版本必须配套）───────────────────
Write-Host '  构建 Zotero 插件 …'
& $py scripts/build-plugin.py --out $pkg --repo 'leobba/litflow' --no-update
if ($LASTEXITCODE -ne 0) { throw '插件构建失败' }

# ── 3. 非二进制资产 ────────────────────────────────────────────────────
Copy-Item 'config.example.json' $pkg -Force
Copy-Item 'start-litflow.ps1'   $pkg -Force
Copy-Item 'start-litflow.cmd'   $pkg -Force
Copy-Item 'stop-litflow.cmd'    $pkg -Force
Copy-Item 'LICENSE'             $pkg -Force -ErrorAction SilentlyContinue

$readmePkg = Join-Path $Root 'docs\QUICKSTART.md'
if (Test-Path $readmePkg) { Copy-Item $readmePkg (Join-Path $pkg 'README.md') -Force }
# 术语表文档也带上：这是个能被用错的功能（表头写错、路径含逗号），
# 而用错的表现是「静默不生效」。
$glossaryDoc = Join-Path $Root 'docs\GLOSSARY.md'
if (Test-Path $glossaryDoc) { Copy-Item $glossaryDoc (Join-Path $pkg 'GLOSSARY.md') -Force }

# ── 4. 说明「包里没有 pdf2zh」──────────────────────────────────────────
# 用户第一次跑会在日志里看到找不到 pdf2zh_next。与其让他猜，写进包里。
$note = @"
pdf2zh_next（PDF 翻译引擎）不在这个包里，原因有两个：

  1. 它是 AGPL v3。只以子进程方式调用（这是允许的），但不能打进本程序一起分发
     而不承担对应的源码提供义务。
  2. 它的运行环境约 850 MB，独立一个目录更合理。

装法（二选一）：

  A) 从面板「环境检测」页点「下载 pdf2zh」（走加速线路，约 592 MB）
     直链 https://github.com/leobba/litflow/releases/download/pdf2zh-v2.9.0/
          pdf2zh-v2.9.0-BabelDOC-v0.6.4-with-assets-win64.zip
  B) pip install pdf2zh-next
    pip install pdf2zh-next
  然后在 config.json 的 translate.pdf2zh_exe 里填它的绝对路径，例如
    "D:\\\\zotero-pdf2zh\\\\server\\\\zotero-pdf2zh-next-venv\\\\Scripts\\\\pdf2zh_next.exe"

不填也行 —— 它会从 PATH 里找 pdf2zh_next。
"@
# 文件名用 ASCII：中文名在 zip 里依赖 UTF-8 标志位，而 Windows 资源管理器的解压
# 对那份标志位的处理并不一致，会出现解压后名字乱码。
Set-Content -Path (Join-Path $pkg 'ABOUT-pdf2zh.txt') -Value $note -Encoding UTF8

# ── 5. 校验：缺任何一件都算打包失败，而不是发出去让人踩 ────────────────
$required = @(
    'wb2api.exe',
    'dualsync@zotero.local.xpi',
    'config.example.json',
    'start-litflow.cmd',
    'stop-litflow.cmd',
    'README.md'
)
$missing = @()
foreach ($f in $required) {
    if (-not (Test-Path (Join-Path $pkg $f))) { $missing += $f }
}
if ($missing.Count) {
    throw "打包缺件: $($missing -join ', ')"
}

# xpi 必须带目录条目，否则 Zotero 会拒绝安装（见 scripts/build-plugin.py 的说明）
& $py -c @"
import sys, zipfile
z = zipfile.ZipFile(r'$pkg\dualsync@zotero.local.xpi')
if not any(n.endswith('/') for n in z.namelist()):
    print('  xpi 缺目录条目'); sys.exit(1)
if z.testzip():
    print('  xpi CRC 失败'); sys.exit(1)
z.close()
"@
if ($LASTEXITCODE -ne 0) { throw 'xpi 自检失败' }

# ── 6. 出 zip ──────────────────────────────────────────────────────────
$zip = Join-Path $dist "litflow-$verNum-windows-x64.zip"
if (Test-Path $zip) { Remove-Item $zip -Force }
Compress-Archive -Path $pkg -DestinationPath $zip -CompressionLevel Optimal

$exeSize = [math]::Round((Get-Item $exe).Length / 1MB, 1)
$zipSize = [math]::Round((Get-Item $zip).Length / 1MB, 1)
Write-Host ''
Write-Host "  目录 $pkg" -ForegroundColor Green
Write-Host "  压缩 $zip  ($zipSize MB)" -ForegroundColor Green
Write-Host "  exe  $exeSize MB（-s -w 剥离后）"
Write-Host ''
Get-ChildItem $pkg -File | Sort-Object Name | ForEach-Object {
    Write-Host ("    {0,-34} {1,9:N0} bytes" -f $_.Name, $_.Length)
}
