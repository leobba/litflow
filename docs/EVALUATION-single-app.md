# 评估：单一软件的目标 vs 现状，以及旧版本的清理方案

> 目标（你定的）：**以 wb2api 源码为基础，做一款「账号管理 + 翻译 + Zotero 打通」的单一软件。**

## 已全部完成（本文件保留为决策记录）

| 项 | 结果 |
|---|---|
| **单一软件** | `D:\workbuddy2api\wb2api.exe` 13.3 MB 单文件。一个进程、一个端口 7863，同时服务三类调用方：OpenAI 兼容 `/v1/*`、面板 `/panel/`、Zotero 插件 `/health`+`/translate` |
| **队列 API 统一** | 面板和插件共用根路径那一套端点（回环免 key、非回环要 key） |
| **Zotero 打通** | 插件 v1.1.0 指向 `:7863`；面板里一键下载 xpi，卡片直接给出**下一步动作** |
| **旧版本清理** | `D:\litflow` 623 MB 已归档（1.1 MB）后删除；`E:\code\LitFlow` 103 MB → 13 MB；tkinter 窗口 1 518 行已删；插件 16 个 `.bak` 已清 |
| **凭证** | `litflow.sqlite3` 的 7 个账号已导出；6 个与现有重复，唯一非重复的 `leobba`（global realm，有效至 2027-09-28）在 `data/legacy/accounts-from-litflow-legacy.json` |
| **许可义务** | OFM 的 MIT 全文已内联进 `E:\code\LitFlow\NOTICES.md`——原文依赖 `_ref/ofm/LICENSE`，而那个目录被删了 |

下面保留评估当时的判断，作为决策依据。

---

## 一、目标拆开看：四块里三块已经有了

| 能力 | 现状 | 在哪 |
|---|---|---|
| **账号管理** | ✅ 完成 | wb2api：OAuth 设备授权、账号池、积分/签到、启停/暂停/移除、面板 6 页 |
| **翻译** | ✅ 完成 | wb2api `internal/translate`：串行队列、静默失败检测、跨模型轮换 |
| **网页前端** | ✅ 完成 | wb2api 面板（`go:embed` 内嵌，单二进制，7 页） |
| **Zotero 打通** | ⚠️ **插件已存在，但没连到 wb2api** | 见第三节 |

所以这不是「从零做一款软件」，而是**把最后一段接线接上**。

---

## 二、四个位置的角色判定

| 位置 | 体量 | 真代码 | 判定 |
|---|---|---|---|
| `D:\workbuddy2api` | 18.4 MB / 196 文件 | ~24 400 行 Go | **基座，继续开发** |
| `E:\code\LitFlow` | 102.8 MB | **1.4 MB**（其余是参考与产物） | **实验室**：文档 + 契约测试 + 未移植逻辑 |
| `D:\litflow` | **623 MB** | 2.2 MB（core） | **上一代成品，主体已被 wb2api 取代** |
| `D:\zotero-dual-sync` | 0.8 MB | 105.8 KB（dualsync.js） | **保留**，清掉 12 个 `.bak` |

### `D:\litflow` 是什么

上一代的**完整产品**：Python `core/app/`（87 文件）+ React/Vite 前端 + 启动脚本 + PLAN/STATUS/HANDOFF。
git 历史停在 2026-10-14，工作树干净。

它做的事情**与 wb2api 高度重叠**，而且 wb2api 用 Go 做得更完整：

| D:\litflow 模块 | 行数 | wb2api 里对应 |
|---|---|---|
| `providers/workbuddy.py` | 2 131 | `internal/upstream`（Go，逆向更完整） |
| `workbuddy_rewards.py` | 1 181 | 成长任务/签到/猫猫旅行 |
| `translate/jobs.py` | 979 | `internal/translate` |
| `workbuddy_session_transfer.py` | 965 | OAuth 设备授权 |
| `pool/state.py` | 938 | `internal/pool` |
| `router/openai_api.py` | 582 | `internal/server` |
| `accounts/service.py` | 752 | `internal/auth` + 面板 |

**但它有两样东西，`E:\code\LitFlow` 和 wb2api 都没有：**

| 模块 | 行数 | 价值 |
|---|---|---|
| **`zotero_api.py`** | 521 | **服务端写 Zotero**：走 `127.0.0.1:23119/api/users/0/items`，把双语 PDF 挂成附件。文档里记了实测：本地 API **默认关闭**（`GET /api/users/0/items` → 403），需要开 `extensions.zotero.httpServer.localAPI.enabled` |
| **`md_export/export.py`** | 513 | **Markdown 导出** —— 这就是你上次问的「文章解析」的雏形 |

---

## 三、Zotero 打通的缺口：**只差 3 个端点 + 1 个端口号**

插件 `dualsync.js`（v1.0.26）只调用 **3 个接口**：

```
GET  /health                 -> {"status":"ok","ready":bool,"models":N}
POST /translate              -> {"id":..., "state":..., "liveness":...}   (期望 202)
GET  /translate/<id>         -> job dict
```

而它的后端地址写死在 prefs 里：

```
pref("extensions.zotero.dualsync.litflowUrl", "http://127.0.0.1:8788");
pref("extensions.zotero.dualsync.backend", "litflow");   // 回退 "script"
```

wb2api 现在**没有**这三个路径上的接口（它的是 `/healthz` 和 `/panel/api/translate`，而且带 Bearer 鉴权）。

**所以缺口是：**

1. 在 wb2api 根路径加 3 个端点，返回 LitFlow 那个形状（约 80 行 Go）
2. 插件改成 `http://127.0.0.1:7863`
3. 端到端验证一次

**注意**：插件期望的 `/translate` 是**无鉴权**的（它不带 api_key 头）。wb2api 对外默认要 `api_key`，所以这 3 个端点要挂在**免鉴权**路径上——但只绑 loopback，且只暴露队列，不暴露账号操作。这个边界要在实现时写清楚。

### 现在插件实际在走什么

实测：**8788 无监听** → 插件当前走的是 **script 回退**（spawn `pythonw` + `batch_translate_live.py`）。
三个回退路径都在（`pythonw.exe`、`batch_translate_live.py`、`backup_db.py`）。

也就是说 **LitFlow 服务现在根本没被用上**，插件的翻译是脚本模式跑的。这反而说明：把它接到 wb2api 上不会打断任何正在工作的东西，只是换一条更好的路。

### 插件还做了哪些事（不用重做，已经在插件里）

- 把 `storage/<KEY>/*dual.pdf` 挂成条目**链接附件**
- 给条目打 `双语已译`(绿) / `待翻译`(红) 标签
- 管理 Zotero **自己的**数据库备份（24 份 × 120 分钟）
- 上传 AList（`127.0.0.1:5244`）

---

## 四、清理方案（分级，不是一刀切）

### A. 可直接删（信息损失为零，共约 **89 MB**）

| 路径 | 大小 | 为什么安全 |
|---|---|---|
| `E:\code\LitFlow\_zotero_peek` | 55.2 MB | 3 个文件：`app.zip`(47M) / `omni.zip`(10M) / `zhttp.js`。<br>是当年用来「窥探」插件契约的第三方二进制；**知识已经落成 `test_plugin_contract.py` 的 24 条断言** |
| `E:\code\LitFlow\_ref` | 20.7 MB | OFM 第三方参考源码。许可证义务在 `NOTICES.md` 里，与这份拷贝无关 |
| `E:\code\LitFlow\_e2e` | 13.2 MB | 我这轮产生的测试产物（截图、日志、临时 profile） |
| `E:\code\LitFlow\_mutation` | 0.5 MB | 变异测试的一次性 scratch，跑测试会重建 |
| `E:\code\LitFlow\_probe` | 0.5 MB | 探针输出 |
| `D:\zotero-dual-sync\*.bak-*` | ~0.25 MB | **7 个 xpi 备份 + 4 个 js 备份 + 5 个 manifest/pref 备份**。<br>它们让「哪个版本是活的」变得不可判定 |

### B. 需要你决定：`E:\code\LitFlow\src\desktop\`（1 518 行）

我上一轮建的 **tkinter 桌面窗口**。现在你要的 UI 是 wb2api 的**网页面板**，所以它被取代了。

- 删：代码干净少 1 500 行，减少「两套 UI」的困惑
- 留：万一以后想要原生窗口（我之前建议 Tauri 推迟，这个是它的平替参考）

**我倾向删**——一个被取代的 UI 留着只会让人以为它有维护。

### C. 必须**归档**而不是删：`D:\litflow`

623 MB 里有 **一个不能盲删的文件**：

```
D:\litflow\data\litflow.sqlite3   548 KB
  accounts      7 行   ← secret_blob 4408 字节明文 + meta_json 里能看到 refresh_token
  jobs         46 行
  usage_log  2422 行
```

**7 个账号的明文凭证。** 里面能看到 `7487fc9f…` —— **和现在 `D:\workbuddy2api\auths` 里那个是同一个账号**，还有一条 `dsh / commandcode` 的 api_key。

所以对 `D:\litflow` 的正确处理是：

| 动作 | 内容 |
|---|---|
| **先导出** | `data/litflow.sqlite3`（或有价值的 subset）+ `core/` + `docs/` → 一个 zip 放 `_archive/` |
| **再删可重建部分** | `.venv` 136 MB、`web/node_modules`（在 106 MB 里）、`cockpit-tools-main` + 其 32 MB zip、`_ref` 125 MB、`_probe` 57.9 MB |
| **然后决定** | 整个 `D:\litflow` 删掉？还是留 core/ 当参考？ |

**我不建议现在就删它**，因为里面有两样 E 和 wb2api 都没有的东西（Zotero 服务端写入、Markdown 导出）。**先把这两块看懂，再删。**

### D. 绝不能删

| 路径 | 为什么 |
|---|---|
| `E:\code\LitFlow\_vendor\workbuddy2api-20261016` | **独立 git 仓库 + MIT LICENSE + `UPSTREAM_COMMIT.txt`**。上游 `Sliverkiss/workbuddy2api` 已被删过一次，这是唯一的备份 |
| `E:\code\LitFlow\docs\` | 本轮全部实测知识（`LIVE-FINDINGS-2.md` / `HANDOFF-2.md` / `INTEGRATION.md`） |
| `E:\code\LitFlow\test\test_plugin_contract.py` | **它现在是 wb2api 那 3 个新端点的规格说明** |
| `D:\workbuddy2api` | 基座 |

---

## 五、建议的开发顺序（等你确认后执行）

| 步 | 内容 | 为什么这个顺序 |
|---|---|---|
| **1** | wb2api 加 3 个插件端点（免鉴权、只限 loopback、只暴露队列） | 最小闭环。有 `test_plugin_contract.py` 当规格，可以直接断言 |
| **2** | 插件改 `litflowUrl` → `7863`，重新打包 xpi | 一行 prefs |
| **3** | 端到端：Zotero 里点「翻译选中」→ wb2api 队列 → 双语 PDF 回挂 | 这是「打通」的验收 |
| **4** | 清理（第四节 A + B） | 等 1-3 通了再清，避免拆掉还在用的东西 |
| **5** | 归档 `D:\litflow`，读它的 `zotero_api.py` / `md_export` | 最后一个做，因为要读懂再决定 |
| **6** | 再谈「文章解析」是否要做、做成什么样 | 依赖 5 的判断 |

---

## 六、需要你确认的 6 个决定

1. **插件那 3 个端点是否免鉴权？**
   插件当前不带 key。我建议：免鉴权但只绑 `127.0.0.1`，且只开队列读写（不开账号操作）。
   *替代方案*：给插件加一个 key 设置项（要重新发 xpi）。

2. **`E:\code\LitFlow` 的 Python 运行时要不要退役？**
   它的翻译/网关/桌面壳都已被 wb2api 取代。剩下真正独有的是：`rates.py`（倍率上限）、`probe.py`（system prompt 探针）、626 条断言、文档。
   我建议：**运行时退役，保留为「实验室」**（文档 + 契约测试 + 未移植逻辑）。

3. **`src/desktop/`（tkinter 窗口）删还是留？** 我倾向删。

4. **`D:\litflow` 怎么处理？**
   选项：(a) 导出 sqlite + core/docs 后整体删；(b) 只删可重建的 300+ MB，留 core/ 当参考；(c) 暂时全留。
   我建议 **(b)**，等你把 `zotero_api.py` 和 `md_export` 读过之后再定 (a)。

5. **那个 7 账号的明文凭证库怎么办？**
   里面有和 `auths/` 重复的账号。要不要先把非重复的那几个（`dsh/commandcode` 等）迁进新系统，再销毁旧库？

6. **文章解析（Markdown 导出）现在做吗？**
   我建议 **先不做**——先把 Zotero 打通跑通一次，那是可交付的；解析是另一个功能面，而且它的失败是静默的（模型会产出「看起来对但不来自原文」的 JSON，必须带引文校验）。

---

## 七、一句话

**目标已经完成 3/4，缺的是「插件指向 7863」这一段接线（约 80 行 Go + 一行配置）。**
清理的大头（89 MB）是零信息损失的；真正需要小心的是 `D:\litflow` 那个含明文凭证的 sqlite，以及它里面两项 E 和 wb2api 都还没有的功能。
