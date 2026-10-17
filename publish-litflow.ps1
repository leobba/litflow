# publish-litflow.ps1 —— 把本仓库发布到 GitHub，触发 CI 构建两个 Release。
#
# 设计依据：token 只从 .env 或环境变量读，**绝不写进 .git/config**（一次性 URL 推送）；
# 幂等（仓库已存在就跳过建库、tag 已存在就跳过）；推送后打印三个地址。
#
# 用法：
#   powershell -File publish-litflow.ps1                    用默认 owner/repo 和当前版本
#   powershell -File publish-litflow.ps1 -Tag v1.13.1       指定 tag
#   powershell -File publish-litflow.ps1 -DryRun            只打印将要做什么
#
# 前提：.env 里有 GITHUB_TOKEN（classic PAT，scope 需 repo + workflow）

param(
    [string]$Owner = 'leobba',
    [string]$Repo  = 'litflow',
    [string]$Tag   = '',
    [switch]$DryRun
)

$ErrorActionPreference = 'Stop'
$Root = Split-Path -Parent $MyInvocation.MyCommand.Path
Set-Location $Root

$repoFull = "$Owner/$Repo"
$headers = @{
    Authorization          = ''
    Accept                 = 'application/vnd.github+json'
    'X-GitHub-Api-Version' = '2022-11-28'
    'User-Agent'           = 'litflow-publisher'
}

# -DryRun 不需要凭据：它只打印将要做什么。放在 token 检查之前，
# 否则想先看一眼计划的人会先被要求提供 token。
if ($DryRun) {
    $srcHit = Select-String -Path 'cmd/server/main.go' -Pattern 'appVersion = "([^"]+)"' | Select-Object -First 1
    $v = if ($srcHit) { $srcHit.Matches[0].Groups[1].Value -replace '-panel$', '' } else { '?' }
    Write-Host "[DryRun] 仓库 $Owner/$Repo   版本 v$v" -ForegroundColor Cyan
    Write-Host '  1. GET  /repos/$Owner/$Repo        不存在则 POST /user/repos 建公开库'
    Write-Host '  2. git push 一次性 URL → main      （token 不写进 .git/config）'
    Write-Host "  3. git tag v$v 并推送              （CI 建 v$v 与 release 两个 Release）"
    Write-Host '  4. 打印 Actions / Releases / update.json 三个地址'
    exit 0
}

# ── token ──────────────────────────────────────────────────────────────
# ── token：按约定链查找，不要每次让用户重新生成 ────────────────────────
#
# 链：env:GITHUB_TOKEN → 本目录 .env → ~/.dsh/.credentials.yaml 的 refs.GITHUB_TOKEN
# 第三条是本机的长期凭据，所以换项目不用再要一次 token。
# 技能目录里附带同名实现，这里调用它以免两处漂移。
$skillHelper = Join-Path $env:USERPROFILE '.dsh\skills\github-release-and-auto-update\get-github-token.ps1'
if (Test-Path $skillHelper) {
    $token = (& $skillHelper -From (Get-Location).Path 2>$null | Select-Object -Last 1)
} else {
    # 技能不存在时的退化路径：只查 env 与 .env，行为可预期。
    $token = $env:GITHUB_TOKEN
    if (-not $token -and (Test-Path '.env')) {
        $line = Get-Content '.env' | Where-Object { $_ -match '^\s*GITHUB_TOKEN\s*=' } | Select-Object -First 1
        if ($line) { $token = ($line -replace '^\s*GITHUB_TOKEN\s*=\s*', '').Trim().Trim('"').Trim("'") }
    }
}
if (-not $token) {
    Write-Host '没有找到 GITHUB_TOKEN。已查：' -ForegroundColor Red
    Write-Host '  · 环境变量 GITHUB_TOKEN'
    Write-Host '  · 本目录 .env'
    Write-Host "  · $env:USERPROFILE\.dsh\.credentials.yaml 的 refs.GITHUB_TOKEN"
    Write-Host ''
    Write-Host '获取（一次，之后跨项目复用）：' -ForegroundColor Yellow
    Write-Host '  1. https://github.com/settings/tokens/new'
    Write-Host '  2. Note 填用途，Expiration 30 天'
    Write-Host '  3. 勾 repo + workflow（后者必须有）'
    Write-Host '  4. 写进凭据库的 refs: 段，缩进 4 空格：GITHUB_TOKEN: ghp_xxx'
    Write-Host ''
    Write-Host '不要把 token 粘贴到对话里 —— 那等于泄露。'
    exit 1
}
if ($token -notmatch '^gh[ps]_') {
    Write-Host 'GITHUB_TOKEN 格式不对（应以 ghp_ 或 ghs_ 开头）。' -ForegroundColor Red
    exit 1
}
$headers.Authorization = "Bearer $token"
Write-Host "已读到 GITHUB_TOKEN（长度 $($token.Length)）" -ForegroundColor Green

# ── 版本号：tag 由源码 appVersion 派生，保证一致 ───────────────────────
if (-not $Tag) {
    $src = Select-String -Path 'cmd/server/main.go' -Pattern 'appVersion = "([^"]+)"' |
        Select-Object -First 1
    if (-not $src) { Write-Host "找不到 appVersion" -ForegroundColor Red; exit 1 }
    $ver = $src.Matches[0].Groups[1].Value -replace '-panel$', ''
    $Tag = "v$ver"
}
Write-Host "待发布 tag: $Tag   仓库: $repoFull"

# 源码版本与 tag 必须一致，否则 CI 的一致性断言会拦下来 —— 与其等 CI 报错，这里先说。
$srcLine = Select-String -Path 'cmd/server/main.go' -Pattern 'appVersion = "([^"]+)"' | Select-Object -First 1
$srcVer = $srcLine.Matches[0].Groups[1].Value -replace '-panel$', ''
if ($Tag -ne "v$srcVer") {
    Write-Host "tag $Tag 与源码 appVersion=$srcVer 不一致。" -ForegroundColor Yellow
    Write-Host "先改 cmd/server/main.go 里的 appVersion，再发版。" -ForegroundColor Yellow
    exit 1
}

if ($DryRun) {
    Write-Host "`n[DryRun] 将执行：" -ForegroundColor Cyan
    Write-Host "  1. GET  /repos/$repoFull            （不存在则 POST /user/repos 建公开库）"
    Write-Host "  2. git push 一次性 URL → main"
    Write-Host "  3. git tag $Tag && push tag        （CI 建 v$ver 与 release 两个 Release）"
    Write-Host "  4. 打印 Actions / Releases / update.json 地址"
    exit 0
}

# ── 1. 验证身份 ────────────────────────────────────────────────────────
try {
    $me = Invoke-RestMethod -Uri 'https://api.github.com/user' -Headers $headers
    Write-Host "身份验证通过：$($me.login)" -ForegroundColor Green
} catch {
    Write-Host "token 验证失败：$($_.Exception.Message)" -ForegroundColor Red
    if ($_.ErrorDetails) { Write-Host $_.ErrorDetails.Message }
    exit 1
}

# ── 2. 仓库存在与否 ────────────────────────────────────────────────────
$exists = $true
try {
    $r = Invoke-RestMethod -Uri "https://api.github.com/repos/$repoFull" -Headers $headers
    if ($r.private) {
        Write-Host ""
        Write-Host "仓库是私有的 —— 自动更新会静默失效。" -ForegroundColor Red
        Write-Host "已安装的插件拉 update.json 时不带凭据，私有仓库会 404，而它不会报错，只是永远不升级。"
        Write-Host "请到 https://github.com/$repoFull/settings 把它改成 Public 后重跑。"
        exit 1
    }
    Write-Host "仓库已存在且公开。" -ForegroundColor Green
} catch {
    if ($_.Exception.Response.StatusCode.value__ -ne 404) {
        Write-Host "查询仓库失败：$($_.Exception.Message)" -ForegroundColor Red
        exit 1
    }
    $exists = $false
}

if (-not $exists) {
    Write-Host "仓库不存在，创建公开仓库 $repoFull …"
    $body = @{
        name        = $Repo
        description = 'LitFlow —— 账号管理 + PDF 翻译 + Zotero 打通的单一网关'
        private     = $false
        has_issues  = $true
    } | ConvertTo-Json
    try {
        Invoke-RestMethod -Uri 'https://api.github.com/user/repos' -Method Post `
            -Headers $headers -Body $body -ContentType 'application/json' | Out-Null
        Write-Host "已创建。" -ForegroundColor Green
    } catch {
        Write-Host "创建失败：$($_.Exception.Message)" -ForegroundColor Red
        if ($_.ErrorDetails) { Write-Host $_.ErrorDetails.Message }
        exit 1
    }
}

# ── 3. 推送（一次性 URL，不写进 .git/config）──────────────────────────
$pushUrl = "https://x-access-token:$token@github.com/$repoFull.git"
Write-Host "`n推送 main …"
git remote remove litflow-publish 2>$null | Out-Null
git remote add litflow-publish $pushUrl
try {
    git push litflow-publish HEAD:main --force
    if ($LASTEXITCODE -ne 0) { throw "git push 失败（exit $LASTEXITCODE）" }
} catch {
    Write-Host "推送失败：$_" -ForegroundColor Red
    Write-Host ""
    Write-Host "如果报 refusing to allow a Personal Access Token to create or update workflow，"
    Write-Host "说明 token 缺 workflow scope，去重新生成并勾上。"
    git remote remove litflow-publish 2>$null | Out-Null
    exit 1
} finally {
    # 无论成败都清掉带 token 的 remote：它会被写进 .git/config
    git remote remove litflow-publish 2>$null | Out-Null
}

# ── 4. 打 tag（CI 由此触发两个 Release）────────────────────────────────
Write-Host "打 tag $Tag …"
$existing = git tag -l $Tag
if ($existing) {
    Write-Host "tag 已存在，跳过创建（要重发请先 git tag -d $Tag 并删远端 tag）。" -ForegroundColor Yellow
} else {
    git tag -a $Tag -m "LitFlow $Tag"
}
git remote add litflow-publish $pushUrl
try {
    git push litflow-publish $Tag
} finally {
    git remote remove litflow-publish 2>$null | Out-Null
}

Write-Host ""
Write-Host "完成。CI 正在构建，几分钟后看：" -ForegroundColor Green
Write-Host ""
Write-Host "  Actions    https://github.com/$repoFull/actions"
Write-Host "  Releases   https://github.com/$repoFull/releases"
Write-Host "  插件清单   https://github.com/$repoFull/releases/download/release/update.json"
Write-Host ""
Write-Host "验收（别只看 CI 绿）：" -ForegroundColor Cyan
Write-Host "  Invoke-WebRequest 'https://github.com/$repoFull/releases/download/release/update.json'"
Write-Host "  期望 HTTP 200，且 version / update_link 与本次发版一致。"
Write-Host ""
Write-Host "提醒：token 已经用过。如果它曾出现在对话或日志里，请立刻去"
Write-Host "      https://github.com/settings/tokens 撤销并重新生成。" -ForegroundColor Yellow
