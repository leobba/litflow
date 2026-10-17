// Default prefs for Zotero Dual Sync.
// Zotero reads these as defaults; anything set in the Config Editor wins.
// The code also carries the same defaults, so a missing prefs.js is harmless.

pref("extensions.zotero.dualsync.autoTranslate", true);
pref("extensions.zotero.dualsync.autoTranslateMax", 3);
// pythonw.exe, NOT python.exe: Zotero launches this through
// Zotero.Utilities.Internal.exec -> nsIProcess.runwAsync, which cannot pass
// CREATE_NO_WINDOW.  A console child launched from Zotero (a GUI process with
// no console) gets a NEW VISIBLE console window per translated item.
// This pref default is what actually decides it -- the DEFAULT_PYTHON constant
// in dualsync.js is only a fallback for when this pref is absent, so editing
// that constant alone changes nothing.
pref("extensions.zotero.dualsync.pythonExe", "D:\\Program Files\\Python\\pythonw.exe");
// 这里原来还有一个 translateScript，指向 batch_translate_live.py 的「脚本后端」。
// 那条路已删除：脚本连的是 http://127.0.0.1:8787/v1 —— 被本网关取代、早已退役的
// 旧面板，跑不通，而且失败只写一行日志。留着这个 pref 只会让人以为还有第二条路。

// 只剩一个后端了。
//   "litflow" - 走 HTTP 找 LitFlow 服务。队列在服务里，所以串行、进度、单实例
//               保护都由一个常驻进程负责，不在这里重写一遍。
// 旧值 "script" 仍然可能留在升级用户的配置里；代码一律当作 litflow，否则那个
// 残留值会让插件找不到任何可用后端，表现为彻底静默不翻译。
pref("extensions.zotero.dualsync.backend", "litflow");
// 7863 = LitFlow 单一软件的端口（账号管理 + 翻译 + 面板）。
  pref("extensions.zotero.dualsync.litflowUrl", "http://127.0.0.1:7863");
// A 300-page book at a low qps can legitimately run for an hour, so the ceiling
// is high and the poll interval is coarse enough not to be a busy loop.
pref("extensions.zotero.dualsync.litflowPollMs", 2000);
pref("extensions.zotero.dualsync.litflowTimeoutMs", 7200000);

// Should an explicit menu action ("translate selected") override the COLLECTION
// exclusion? On by default, because this plugin's own principle is that skipping
// a paper the user wanted is worse than translating one they did not, and a
// click on one item is a more specific instruction than a collection rule.
// Automatic passes always honour the exclusion regardless of this pref, so the
// bulk protection is unaffected. Set false to make the rule absolute.
//
// A TAG exclusion (config.json -> exclude_tag, default 不翻译) is NOT affected:
// that is a per-item deliberate mark and is respected either way.
pref("extensions.zotero.dualsync.manualBypassesCollectionExclude", true);

pref("extensions.zotero.dualsync.autoSync", true);
pref("extensions.zotero.dualsync.pollSeconds", 300);
pref("extensions.zotero.dualsync.linkMode", "linked");
pref("extensions.zotero.dualsync.managedDeleteSource", false);
pref("extensions.zotero.dualsync.legacySearchesRemoved", false);

// Database backup: this plugin does not implement backup itself - it configures
// Zotero's own engine (which is the only thing that can safely snapshot a LIVE
// database) and exposes a manual trigger + a status file for the uploader.
pref("extensions.zotero.dualsync.manageBackupPrefs", true);
// retention is a COUNT of snapshots; interval is in MINUTES (Zotero's own unit).
// 24 copies at 120 min = up to 48 h of local history.
pref("extensions.zotero.dualsync.localBackupCopies", 24);
pref("extensions.zotero.dualsync.backupIntervalMinutes", 120);
// "立即备份数据库" menu action: also upload to the cloud right away, rather
// than waiting up to 6 h for the scheduled task.
pref("extensions.zotero.dualsync.uploadOnManualBackup", true);
pref("extensions.zotero.dualsync.backupScript", "D:\\zotero-tools\\backup_db.py");
// warn at startup when the backup chain has not run / failed;
// warnMode "log" turns the dialog off while keeping the log line.
pref("extensions.zotero.dualsync.checkBackupOnStartup", true);
pref("extensions.zotero.dualsync.backupWarnAfterHours", 48);
pref("extensions.zotero.dualsync.backupWarnMode", "alert");
// one-shot self tests of the backup code paths at startup (leave false)
pref("extensions.zotero.dualsync.testBackupOnStartup", false);
pref("extensions.zotero.dualsync.testStatusOnStartup", false);
