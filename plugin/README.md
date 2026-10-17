# Zotero Dual Sync v1.1.0

把 `storage/<KEY>/` 里已有的 `*dual.pdf`（pdf2zh 生成的中英对照件）挂成条目附件，
并给条目打 `双语已译`(绿) / `待翻译`(红) 标签。

---

## 安装

1. **工具 → 插件 → 右上角齿轮 → Install Add-on From File…**
2. 选 `D:\zotero-dual-sync\dualsync@zotero.local.xpi`
3. **完全退出 Zotero 再重开**（bootstrap 插件必须重启生效）

> ⚠️ 清单里必须保留 `applications.zotero.update_url`。
> Zotero 10 缺少该字段时会**静默删除** xpi 并报
> 「无法安装插件…它可能无法与该版本的 Zotero 兼容」。
> 详见 `D:\zotero文献\_REPAIR_REPORT_20260921.md` 第八节。

## 使用

- 菜单：**工具 → Dual Sync：同步 dual 并着色**
- 菜单：**工具 → Dual Sync：统计已译/待译**
- 启动 15 秒后自动同步一次，之后每 5 分钟静默同步一次
- 条目列表左侧色条：绿 = 已译，红 = 待译

关闭后台轮询（菜单仍然可用）—— 高级 → 配置编辑器：

```
extensions.zotero.dualsync.autoSync = false
```

## 控制台

```js
Zotero.DualSync.syncNow()
Zotero.DualSync.showStats()
```

---

## 1.0.26 — 排除规则的三个问题

起因：1.0.25 修好 `_attempted` 之后，日志终于露出了**第二个**拦截原因：

```
[DualSync] auto-translate: re-queued 1 item(s) that a previous pass had already attempted
[DualSync] auto-translate: skip item 803 (collection excluded: 3PRCEFLK)   ← 真正的拦截
```

条目在排除分类里。但这暴露了三件事：

### 1. 手动点击应该越过**分类**排除

插件自己的注释是一条设计原则：

> Skipping a paper the user wanted is worse than translating one they did not.
> （漏翻用户想要的，比多翻一个他不想要的更糟）

按这条原则，点一个条目是比分类规则更具体的指令。所以：

- **手动菜单动作越过分类排除**（并写日志说明）
- **自动轮次照旧遵守**，所以批量保护没有削弱
- 想让它绝对化：`extensions.zotero.dualsync.manualBypassesCollectionExclude = false`

### 2. 标签排除是个回归（真问题）

旧批处理脚本里有 `标签不翻译 -> {tag}` 检查；**LitFlow 侧没有任何等价检查**。
所以切到 LitFlow 后端后，带 `不翻译` 标签的条目**会被翻译**。

现在由插件在 drain 里执行这个检查。标签是**逐条目的刻意标记**（「这篇不翻译」），
所以它**不受**上面的绕过开关影响，两种模式下都遵守。

### 3. 被拒绝时必须说话

这是这个插件反复踩的同一个坑：对明确指令的静默拒绝。

- `!waiting.length` 的静默返回已在 1.0.25 修掉
- 现在再加一条：菜单点击后 4 秒内若**没有任何条目开始**，弹窗说明原因
  （哪条被排除、排除名单在哪个文件、标签要怎么处理）

---

## 1.0.25 — 「点了翻译没反应」的根因

**症状**：菜单点击后什么都没有。日志里只有 `[DualSync] translate selected: 1 item(s)`
和 `/health` 探测成功，之后一片空白——连一条 `skip item` 都没有。

**根因**：`_attempted` 是本会话「只试一次」的守卫，且**从不重置**。
Zotero 启动时的自动排空已经把这些条目标记为 `_attempted`，于是手动点击时：

```js
var waiting = Object.keys(_pending).filter(function (id) { return !_attempted[id]; });
if (!waiting.length) { _draining = false; return; }   // ← 这里没有任何日志
```

`waiting` 为空 → **静默返回**。

这个守卫本意是防止重试循环，但它把**人的明确指令**也一起挡掉了。

**修法两处**：

1. `force`（即菜单动作）会清掉所选条目的 `_attempted`，并记录
   `re-queued N item(s) that a previous pass had already attempted`。
   ——**人主动要求不是循环。**
2. `!waiting.length` 现在**一定会打日志**，说明「有 N 个待办但本会话都试过了」。
   静默返回正是让这个问题无法诊断的原因。

---

## 1.0.24 — LitFlow 后端（以及为什么保留旧路径）

翻译现在默认走 **LitFlow 服务**（`http://127.0.0.1:7863`），不再每个条目 spawn 一次
`pythonw batch_translate_live.py`。

```
extensions.zotero.dualsync.backend = "litflow" | "script"
extensions.zotero.dualsync.litflowUrl = "http://127.0.0.1:7863"
```

**为什么换**：旧路径每个条目起一个进程，所以「防止两个 babeldoc 同时跑」这类
守卫必须在插件里重新实现一遍，并且要从外面保证它是对的。常驻服务**天然只有一份
队列**，是便宜得多的正确方式。串行是要求而非限制——两个 babeldoc 实测各约 1.8GB。

插件这边只做三件事：探测 `/health` → `POST /translate` → 轮询 `GET /translate/<id>`。
它不再有自己的队列、互斥锁或超时账本。

**旧路径没有删。** `backend = "script"` 一键切回；而且 `litflow` 模式下如果服务
不应答，插件会在日志里说明并**自动退回** script，而不是静默什么都不做。

### 两个必须知道的细节

**1. `errorDelayMax: 0` 是必需的，不是防御性的。**

`Zotero.HTTP.request` 会**自动重试 429/5xx**，延迟 2.5s→5s→…→300s，默认上限
**一小时**（`xpcom/http.js` 的 `_errorDelayIntervals` / `_errorDelayMax`）。
LitFlow 在所有候选都冷却时返回 503，本意就是让调用方稍后再来——但按默认行为，
Zotero 会为此**静默重试一小时**，插件看起来就是卡死。所以我们关掉重试
（`errorDelayMax: 0` + `noRetryOnThrottle: true`，后者覆盖带 `Retry-After` 的
429/503），自己在应用层等待。

**2. 进度是粗的，这是 PDF2ZH 的限制，不是我们的。**

pdf2zh 用 rich 渲染阶段表格，而 rich 在 stdout 非 TTY 时会**关闭实时显示**。
实测（147KB 输出、逐行搜 `-----`）：管道模式下阶段条出现 **0 次**，且没有任何
环境变量能恢复（`TERM` / `FORCE_COLOR` / `COLUMNS`+`LINES` / `PYTHONUNBUFFERED`
全部试过）。

所以服务上报的是**已用时间**而不是百分比——「running 28s」回答了用户中途唯一
的问题：它在干活还是卡死了。**不编一个定时器驱动的假进度条。**

### 回滚

```powershell
# 切回旧后端，重启 Zotero
# 高级 -> 配置编辑器: extensions.zotero.dualsync.backend = "script"
```

文件级回滚：`content\dualsync.js.bak-before-litflow-*` 与
`dualsync@zotero.local.xpi.bak-before-litflow-*`。

---

## 1.0.4 修复了什么

这一版修掉了 6 个真实缺陷（都不是"风格问题"）：

| # | 问题 | 后果 | 修法 |
|---|---|---|---|
| 1 | 代码读 `att.attachmentTitle` | **该属性在 `Zotero.Item` 上不存在**（只是 `<attachment-row>` 元素的 getter），永远读到 `undefined`，按标题识别 dual 的分支是从未生效的死代码 | 改用 `att.getField("title")` |
| 2 | `ensureTagColors()` 每次同步都无条件 `setColor` | `tagColors` 是**会同步的设置**，每 5 分钟白写一次库 + 同步抖动 | 改为先读后比，仅变化时写 |
| 3 | `removeUnwantedSearches()` 每次同步都删同名保存检索，且名单里含 `双语已译`/`待翻译` | 会静默删掉你自己建的检索 | 只删历史上的 emoji 版检索名；加 `dualsync.legacySearchesRemoved` 前置开关，**只跑一次** |
| 4 | `linkDual()` 失败时回退 `Zotero.Attachments.importFromFile()` | 该 API 会**复制**一份 PDF 到全新 storage 目录；配合 5 分钟定时器会持续堆重复文件 | 删掉回退，链接失败就上报，**绝不静默复制** |
| 5 | `onMainWindowLoad` 用 `String(window)` 当去重 key | 所有窗口都是 `"[object ChromeWindow]"`，**只有第一个窗口能挂上菜单** | 改用 `WeakSet` 按窗口对象去重 |
| 6 | `shutdown()` 不取消 15 秒定时器 | 插件卸载后仍可能触发一次同步 | 用 `_syncTimeout` 记录并清除 |

顺带：版本号不再写死在日志里；`init` 会清掉上一次的定时器（重载时不会叠加）。

---

---

## 1.0.5 / 1.0.6 改了什么

### 触发方式（1.0.6）

| 机制 | 说明 |
|---|---|
| 启动一次 | Zotero 启动 15 秒后扫一遍 |
| **文件系统轮询** | 默认 **300 秒**。**不能删掉** —— 外部流水线往 `storage/` 写 dual 文件时，Zotero **不产生任何事件**，只有轮询能发现它 |
| **`Zotero.Notifier`** | 监听 `item` 的 `add`/`modify`，去抖 3 秒后同步。让 **Zotero 内部**的改动（导入、手动加附件）立刻生效，不必等下一次轮询 |
| 菜单 | 手动触发，随时可用 |

**轮询变便宜了**：每次只 `stat` 各 storage 子目录的 mtime，**mtime 未变就跳过整个目录**的逐文件列举，并复用上次的判定结果（所以汇总计数仍然准确）。条目上千后这也是常数级开销。

实测：连续两次完整同步后，附件数 / 条目数 / dual 附件数**完全不变**（幂等，不会堆重复）。

### 附件挂载方式可配（1.0.5）

| 配置项 | 默认 | 含义 |
|---|---|---|
| `extensions.zotero.dualsync.linkMode` | `linked` | `linked` = 指向 dual 原位置。**不复制**，但 Zotero **文件同步不会上传链接附件**，且存的是绝对路径。<br>`managed` = 让 Zotero **复制**进自己的 storage 目录，会参与文件同步，但多占一份空间 |
| `extensions.zotero.dualsync.managedDeleteSource` | `false` | 仅 `managed` 模式：导入后删除原 dual 文件（避免双份） |
| `extensions.zotero.dualsync.pollSeconds` | `300` | 轮询间隔秒数；`0` = 关闭轮询（此时只有启动同步 + 菜单 + 通知器） |
| `extensions.zotero.dualsync.autoSync` | `true` | `false` = 完全关闭后台工作 |

> ⚠️ 免费 Zotero 账号的文件同步配额只有 **300 MB**，而 dual 动辄 10–40 MB。
> 切到 `managed` 前先算一下配额 —— 这可能是**保持 `linked` 反而更合适**的原因。

---

## 自动翻译（1.0.7+）

**新英文 PDF 入库 → 自动翻译 → 自动挂上并打标签**，全程无需人工。

```
① 新附件入库 → Notifier 触发同步 → 发现"英文 PDF 且无 dual" → 启动翻译
② Zotero 启动补偿扫描 → 关闭期间入库的也补译
③ 翻译产出 dual → 插件挂附件 + 打 双语已译
```

判定复用的是原有那套逻辑（同步时算出的 `todoIds`），不是另写一份。

| pref | 默认 | 说明 |
|---|---|---|
| `extensions.zotero.dualsync.autoTranslate` | `true` | 总开关 |
| `extensions.zotero.dualsync.autoTranslateMax` | `3` | **每会话上限**，超出保持 `待翻译` 标签 |
| `extensions.zotero.dualsync.pythonExe` | `D:\Program Files\Python\python.exe` | 可改 |
| `extensions.zotero.dualsync.translateScript` | `D:\zotero-tools\pdf2zh\batch_translate_live.py` | 可改 |

菜单 **工具 → Dual Sync：翻译选中条目** 可手动批量（**忽略上限**）；API 为 `Zotero.DualSync.translateSelected()`。

防护：**严格串行**（一次一个任务，两个 babeldoc 进程会抢内存）、每条目每会话只试一次、会话上限、路径缺失时明确写日志。

---

## 数据库备份（1.0.10+）

**这个插件不自己实现备份。** Zotero 已经有一个能安全处理**活动数据库**的引擎
（`Zotero.DB.backUpDatabase`：等事务、在线增量、内容没变就跳过），而外部脚本做同样的事会卡死
（我实测两次：240s / 180s）。所以**快照必须由 Zotero 自己做**。

插件负责三件事：

1. **把保留份数/间隔收拢到一处**（写进 Zotero 原生 pref）：
   | pref | 默认 | 作用 |
   |---|---|---|
   | `dualsync.localBackupCopies` | 12 | → `backup.numBackups`（上限 24）|
   | `dualsync.backupIntervalHours` | 6 | → `backup.interval`（分钟）|
   | `dualsync.manageBackupPrefs` | true | 设 false 则不动 Zotero 的备份 pref |
2. **菜单「立即备份数据库」** —— 点一下就是**完整备份**，不只是本地快照：
   ```
   Zotero.DB.backUpDatabase({force:true})   ← Zotero 引擎做快照（唯一能安全处理活动库的方式）
   → exec python backup_db.py --force       ← 立刻上传到夸克
   → 弹窗报告：本地快照名/大小 + 云端上传成功或失败原因
   ```
   上传由外部脚本完成，**插件从不接触 WebDAV 密码**（脚本用 DPAPI 自己读）。
   设 `dualsync.uploadOnManualBackup = false` 则只做本地快照。

3. **菜单「数据库备份状态」** → 弹出一个**中文可读**的摘要（不是原始 JSON）：
   ```
   数据库备份：正常 ✓

   【本地快照】
     最新：zotero.sqlite.bak（1.72 MB）
     时间：2026-09-23 11:24  23 分钟前
     设置：保留 24 份，每 120 分钟一次
     校验：integrity ok, 210 items
   【云端备份（夸克网盘）】
     最近上传：2026-09-23 11:46  1 分钟前
     云端现有：2 份
   【备份链】
     上次运行：2026-09-23 11:46（成功）  触发：计划任务 / 命令行
   ```
   有任何问题时，同一份报告会自动变成"有问题 ✗"，并列出**问题**和**怎么办**四步。

4. **启动时自检**（`checkBackupOnStartup`，默认开）：30 秒后检查备份链是否健康，
   不健康就弹窗。判定的是**链路本身**而不是"有没有传文件"——因为"没东西可传"是成功而非失败：
   | 情况 | 动作 |
   |---|---|
   | 状态文件不存在 | 警告：备份链从未成功运行过 |
   | `lastRunOk === false` | 警告 + 具体错误 |
   | `lastRunAt` 超过 `backupWarnAfterHours`（默认 48h） | 警告：计划任务可能被停用 |
   | 从未成功上传过 | 警告 |
   | 一切正常 | 只写一行日志 `backup: health OK` |

   把 `dualsync.backupWarnMode` 设成 `"log"` 可关掉弹窗、只留日志。

### 上云那一半不在插件里

上传由 `D:\zotero-tools\backup_db.py` + 计划任务「Zotero DB Backup Upload」负责，
原因是**凭证**：WebDAV 密码存在 Zotero 的登录管理器里、用 OSKeyStore 加密，外部脚本拿不到；
而把它写到配置文件里正是我们一直在消灭的反模式。所以脚本用 **Windows DPAPI**（用户绑定加密）存一次。

```powershell
python D:\zotero-tools\backup_db.py --set-password   # 一次性，密码不进明文
python D:\zotero-tools\backup_db.py --check
python D:\zotero-tools\backup_db.py --dry-run
reboot? no -- 直接等计划任务，或 python D:\zotero-tools\backup_db.py
```

脚本做：找最新 `.bak` → **`integrity_check` 通过才上传** → 打包（DB + prefs.js + extensions.json + 恢复说明）
→ PUT 到 Alist → 云端保留（14 日 + 8 周）→ 写状态文件 → 日志 + 非零退出码。

**快照没变就跳过**（用签名比对），避免每次重复上传同一个文件。

### ⚠️ 计划任务注册踩过的三个坑（都已在 `register_backup_task.ps1` 里修好）

| 坑 | 症状 | 根因 |
|---|---|---|
| `schtasks /sc hourly /mo 6` | 几乎不按计划跑 | 生成 `<Interval>PT6H</Interval><Duration>PT10M</Duration>` —— **重复窗口只有 10 分钟** |
| 用 `/change /du 24:00` 去修 | 能跑，但**某天之后彻底不再运行** | 重复窗口锚在 `StartBoundary`，24 小时一到触发器**永久过期** |
| `/tr "\"D:\Program Files\...\python.exe\" ..."` | `Invalid argument/option` | `schtasks /tr` 无法表达嵌套引号（路径里有空格） |

**正确做法**（无需管理员）：
```
每日触发器 + 24 小时重复窗口  →  每天自动续期，永不过期
/tr 指向 D:\zotero-tools\run_backup.cmd（无空格路径）
```
实测：5 分钟间隔下任务在 11:35:01 和 11:40:01 **自行触发**，`result=0`。

> 注意：通过 `/xml` 注册（唯一能设 `StartWhenAvailable` 的方式）**需要管理员权限**。
> 本机是台式机无电池，所以 `DisallowStartIfOnBatteries` 无关紧要；笔记本上则要注意这一条。

### 状态文件的写入规则

插件和上传脚本**共同写** `backup_status.json`，所以写入必须是**合并**而不是替换 ——
否则一方会把另一方的字段抹掉（这真的发生过：手动备份成功后，自检立刻报"从未成功上传过到云端"）。
同理，凡是"谁最后跑的"这类字段都由**实际运行者**在每次运行时刷新，避免读到陈旧值。

## 1.0.7 – 1.0.11 修了什么

| 版本 | 问题 | 后果 |
|---|---|---|
| 1.0.7 | 每次同步都启动**自己的一条**翻译链 | 两个条目会**并发翻译**（各占约 1.8 GB） |
| 1.0.8 | 队列项没有可用 PDF 时**静默丢弃** | 看起来像"功能没运行"，日志里什么都没有 |
| 1.0.9 | **扫描把已删除的条目也算进 已译/待译** | 计数虚高（已译 26 实为 16），并在回收站条目上打标签 |
| 1.0.9 | **`getPref` 读错了 pref 名字** | 见下 |

### ⚠️ 那个 pref bug 值得单独记一笔

Zotero 的真实实现（`chrome/content/zotero/xpcom/prefs.js`）：

```js
function get(pref, global) {
    pref = global ? pref : ZOTERO_CONFIG.PREF_BRANCH + pref;   // PREF_BRANCH = "extensions.zotero."
```

**`global: true` 表示"这个名字已经是完整的"**，而不是"帮我加上 `extensions.zotero.` 前缀"——正好和直觉相反。
我原来写的是 `Zotero.Prefs.get(name, true)`，于是去找一个根本不存在的裸 pref `dualsync.xxx`
→ 返回 `undefined` → **所有 pref 都静默回落到代码默认值**。

现象：`autoTranslateMax` 无论 prefs.js 里写什么都记成 `3`。
修法：`Zotero.Prefs.get(name)`（省略该参数才会加前缀）。

> 这个 bug 只有真跑起来看日志才能发现 —— 代码看起来完全正常。

## 已知限制（没修，需要你知道）

- **dual 是"链接附件"（绝对路径）**，与主库原件的托管附件（`storage:` 相对路径）模型不一致。
  链接附件**不参与 Zotero 文件同步**，目录一移动就失效。
  要彻底解决需要改成托管附件或规定的相对路径方案。
- **只处理用户库**（`libraryType === "user"`），群组库 `binnong` 不会被扫。
- 每 5 分钟全量扫一遍 `storage/`。条目上千后建议改成 `Zotero.Notifier` 事件驱动。
- `strict_max_version: "10.*"` —— Zotero 11 发布后需要上调。
- 图标是纯色占位 PNG。

## 安装文件

- xpi：`D:\zotero-dual-sync\dualsync@zotero.local.xpi`
- 源码：`D:\zotero-dual-sync\`
