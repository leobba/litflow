/* global Zotero, Components, Services, IOUtils */
/* eslint-disable no-undef */

/**
 * Zotero Dual Sync v1.0.4
 * - Links existing `*dual.pdf` files as attachments and colour-codes items.
 * - Never throws out of public APIs.
 * - Tags live on ITEMS (no sidebar searches are created or removed except a
 *   one-time cleanup of the legacy ones this plugin used to make).
 *
 * Hardening in 1.0.4 (see _REPAIR_REPORT for details):
 *   1. `att.attachmentTitle` never existed on Zotero.Item -> detection was dead
 *      code; now uses getField('title').
 *   2. ensureTagColors() is read-before-write (tagColors is a SYNCED setting).
 *   3. removeUnwantedSearches() runs once, guarded by a pref, and no longer
 *      touches names that merely collide with our tag names.
 *   4. linkDual() no longer falls back to importFromFile(), which silently
 *      copied the PDF into a new storage folder.
 *   5. onMainWindowLoad keys its guard by window object (WeakSet) instead of
 *      String(window) — every window used to collapse to one key.
 *   6. shutdown() cancels the 15s timer; auto-sync is switchable via the
 *      pref extensions.zotero.dualsync.autoSync.
 */

(function () {
    if (typeof Zotero === "undefined") {
        return;
    }

    var MENU_SYNC = "dualsync-menuitem-sync";
    var MENU_STATS = "dualsync-menuitem-stats";
    var MENU_TRANSLATE = "dualsync-menuitem-translate";
    var MENU_BACKUP = "dualsync-menuitem-backup";
    var MENU_BACKUP_STATUS = "dualsync-menuitem-backup-status";
    var TAG_DONE = "双语已译";
    var TAG_TODO = "待翻译";
    // 「翻译失败」标签。为什么要单独一个：在此之前，翻译失败在 Zotero 里**不留任何
    // 痕迹** —— 后端不通、pdf2zh 报错、被限流，全都只写进日志，用户看到的是
    // 「点了没反应」。标签是现成的可见通道：能筛选，也能一眼看出哪些该重试。
    var TAG_FAILED = "翻译失败";
    // Zotero 7+/10 expects a hex CSS color *string* here.
    // (Zotero 6 used numeric palette indices 1-9; writing a number makes
    //  itemTreeRow.js throw `color.toLowerCase is not a function`.)
    var COLOR_DONE = "#00A651";
    var COLOR_TODO = "#FF0000";
    // ONLY the searches this plugin used to auto-create. Names that merely
    // collide with our tag names are deliberately NOT included: a user-made
    // saved search must never be deleted behind their back.
    var UNWANTED = ["📌 待翻译英文文献", "✅ 双语已译", "待翻译英文文献", "已译英文文献"];

    var _running = false;
    var _lastSummary = "尚未同步";
    var _timer = null;
    var _syncTimeout = null;
    var _debounce = null;
    var _notifierRef = null;
    // folder key -> mtime (ms). Lets a sync pass skip storage folders whose
    // contents have not changed instead of re-listing every file every time.
    var _folderState = {};
    // WeakSet keyed by the actual window object. (The old code used
    // String(window), which is "[object ChromeWindow]" for every window, so
    // only the first window ever got the menu.)
    var _hookedWindows = typeof WeakSet !== "undefined" ? new WeakSet() : null;
    // The legacy sidebar-search cleanup is a ONE-TIME migration, not a
    // recurring destructive action.
    var LEGACY_CLEANUP_PREF = "dualsync.legacySearchesRemoved";
    // extensions.zotero.dualsync.autoSync = false stops background work
    // (the menu still works). pollSeconds changes the filesystem poll period.
    var AUTO_SYNC_PREF = "dualsync.autoSync";
    var SYNC_INTERVAL_MS = 5 * 60 * 1000;
    var POLL_SECONDS_PREF = "dualsync.pollSeconds";
    // How a discovered dual gets attached:
    //   "linked" (default) = point at the file where it already sits. No copy,
    //            but Zotero does NOT upload linked files, and the stored path
    //            is absolute.
    //   "managed"          = let Zotero import (COPY) it into its own storage
    //            folder so it does participate in file sync. That duplicates
    //            the file unless managedDeleteSource is also true.
    var LINK_MODE_PREF = "dualsync.linkMode";
    var MANAGED_DELETE_SOURCE_PREF = "dualsync.managedDeleteSource";
    var NOTIFIER_ID = "dualsync";
    var NOTIFY_DEBOUNCE_MS = 3000;

    // ------------------------------------------------------- auto-translation
    // Until 1.0.7 the plugin only ATTACHED duals that already existed - nothing
    // ever produced them, so a new PDF sat untranslated until someone ran the
    // Python pipeline by hand. These prefs let the plugin start that pipeline
    // itself when an English PDF without a dual turns up (a new attachment via
    // the notifier, or the catch-up scan at startup).
    var AUTO_TRANSLATE_PREF = "dualsync.autoTranslate";
    var TRANSLATE_MAX_PREF = "dualsync.autoTranslateMax";
    var PYTHON_PREF = "dualsync.pythonExe";
    // Defaults only - override in the Config Editor (they are pref-backed so
    // nothing here is a hard requirement).
    // pythonw.exe, NOT python.exe: Zotero launches this through
    // Zotero.Utilities.Internal.exec -> nsIProcess.runwAsync, which cannot pass
    // CREATE_NO_WINDOW.  A console child (python.exe) launched from Zotero (a GUI
    // process with no console) gets a NEW VISIBLE console window — measured: the
    // child reports GetConsoleWindow() != 0 and IsWindowVisible() == true.  With
    // pythonw.exe the child reports hwnd=0, no window.
    // Trade-off: pythonw discards the child's stdout/stderr.  This path never read
    // them (exec only resolves true/false), and the batch script still writes
    // progress.md / batch_log.json / logs/log_<key>.txt to disk.
    var DEFAULT_PYTHON = "D:\\Program Files\\Python\\pythonw.exe";
    // Which backend drives translation, and where the LitFlow service lives.
    // See prefs.js for why litflow is the default and why it falls back.
    var BACKEND_PREF = "dualsync.backend";
    var LITFLOW_URL_PREF = "dualsync.litflowUrl";
    var LITFLOW_POLL_PREF = "dualsync.litflowPollMs";
    var LITFLOW_TIMEOUT_PREF = "dualsync.litflowTimeoutMs";
    var DEFAULT_BACKEND = "litflow";
    // 网关地址。7863 = LitFlow 单一软件的端口（账号管理 + 翻译 + 面板都在这里）。
  // 从前是 8788，那个端口属于独立的 Python 服务，已经并进网关。
  var DEFAULT_LITFLOW_URL = "http://127.0.0.1:7863";
    // Whether an explicit menu action overrides the COLLECTION exclusion. On by
    // default: this file's own principle is that skipping a paper the user
    // wanted is worse than translating one they did not, and a click on one
    // item is a more specific instruction than a collection rule. Automatic
    // passes always honour the exclusion. Set false to make the rule absolute.
    var MANUAL_BYPASS_PREF = "dualsync.manualBypassesCollectionExclude";
    var _translating = {};        // storage key -> true while its job runs
    var _pending = {};            // itemID -> waiting for a translation slot
    var _attempted = {};          // itemID -> already tried this session
    var _translateStarted = 0;    // jobs started this session
    //: itemID -> why it was skipped, for the current explicit request. Lets the
    //: menu action say something instead of leaving the user with silence,
    //: which is what "I clicked translate and nothing happened" always is.
    var _skippedForExclusion = {};
    var _draining = false;        // exactly one drain loop at a time

    // ------------------------------------------------------------ db backup
    // The plugin does NOT implement backup itself - Zotero already has a proper
    // engine (Zotero.DB.backUpDatabase) that handles a LIVE database: it waits
    // for transactions, does an online incremental copy, and skips when nothing
    // changed. An external script doing this hangs (verified twice: 240s/180s),
    // so the snapshot must happen in here.
    //
    // What this adds is: the two retention/interval knobs in one place, a
    // trustworthy manual trigger, and a readable status file that an external
    // uploader + watchdog can consume.
    var LOCAL_COPIES_PREF = "dualsync.localBackupCopies";
    var BACKUP_MINUTES_PREF = "dualsync.backupIntervalMinutes";
    var BACKUP_SCRIPT_PREF = "dualsync.backupScript";
    var UPLOAD_ON_MANUAL_PREF = "dualsync.uploadOnManualBackup";
    var WARN_AFTER_HOURS_PREF = "dualsync.backupWarnAfterHours";
    var WARN_MODE_PREF = "dualsync.backupWarnMode";   // "alert" | "log"
    var DEFAULT_BACKUP_SCRIPT = "D:\\zotero-tools\\backup_db.py";
    var STATUS_FILE = "D:\\zotero-tools\\backup_status.json";
    var _backupWarned = false;      // warn at most once per session
    var _healthTimeout = null;

    function log(msg) {
        try {
            Zotero.debug("[DualSync] " + msg);
        } catch (e) {}
    }

    function isDualFilename(filename) {
        try {
            var n = String(filename || "").toLowerCase();
            if (n.indexOf(".pdf") === -1) return false;
            return (
                n.slice(-9) === ".dual.pdf" ||
                n.slice(-9) === "-dual.pdf" ||
                n.indexOf("-side_by_side.pdf") !== -1 ||
                n.indexOf("no_watermark.zh-cn.dual.pdf") !== -1
            );
        } catch (e) {
            return false;
        }
    }

    function isOriginalPdf(filename) {
        try {
            var n = String(filename || "").toLowerCase();
            return n.slice(-4) === ".pdf" && !isDualFilename(n) && n.indexOf("no_watermark.zh") === -1;
        } catch (e) {
            return false;
        }
    }

    function basename(path) {
        var parts = String(path || "").split(/[\\/]/);
        return parts[parts.length - 1] || "";
    }

    function joinPath(base, name) {
        var b = String(base).replace(/[\\/]+$/, "");
        var sep = "/";
        try {
            if (Zotero.isWin) sep = "\\";
        } catch (e) {}
        return b + sep + String(name).replace(/^[\\/]+/, "");
    }

    function looksEnglish(text) {
        var t = String(text || "");
        if (!t) return false;
        if (/[一-鿿]/.test(t)) return false;
        return /[A-Za-z]{3,}/.test(t);
    }

    function isRegular(item) {
        if (!item) return false;
        try {
            if (typeof item.isRegularItem === "function") return !!item.isRegularItem();
        } catch (e) {}
        try {
            if (typeof item.isAttachment === "function" && item.isAttachment()) return false;
            if (typeof item.isNote === "function" && item.isNote()) return false;
        } catch (e) {}
        return false;
    }

    function itemTitle(item) {
        try {
            return item.getField("title") || "";
        } catch (e) {
            return "";
        }
    }

    function toFile(path) {
        try {
            return Zotero.File.pathToFile(path);
        } catch (e) {
            return null;
        }
    }

    function listChildren(path) {
        return new Promise(function (resolve) {
            var out = [];
            Promise.resolve()
                .then(function () {
                    if (typeof IOUtils !== "undefined" && IOUtils.getChildren) {
                        return IOUtils.getChildren(path);
                    }
                    return null;
                })
                .then(function (children) {
                    if (children && children.length !== undefined) {
                        resolve(children);
                        return;
                    }
                    try {
                        var file = toFile(path);
                        if (!file || !file.exists() || !file.isDirectory()) {
                            resolve([]);
                            return;
                        }
                        var en = file.directoryEntries;
                        while (en.hasMoreElements()) {
                            out.push(en.getNext().QueryInterface(Components.interfaces.nsIFile).path);
                        }
                        resolve(out);
                    } catch (e) {
                        resolve([]);
                    }
                })
                .catch(function () {
                    resolve([]);
                });
        });
    }

    function isDirectory(path) {
        return new Promise(function (resolve) {
            Promise.resolve()
                .then(function () {
                    if (typeof IOUtils !== "undefined" && IOUtils.stat) {
                        return IOUtils.stat(path);
                    }
                    return null;
                })
                .then(function (st) {
                    if (st && st.type) {
                        resolve(st.type === "directory");
                        return;
                    }
                    try {
                        var f = toFile(path);
                        resolve(!!(f && f.exists() && f.isDirectory()));
                    } catch (e) {
                        resolve(false);
                    }
                })
                .catch(function () {
                    resolve(false);
                });
        });
    }

    function findItemByKey(key) {
        return Promise.resolve()
            .then(function () {
                var libs = Zotero.Libraries.getAll();
                var i = 0;
                function next() {
                    if (i >= libs.length) return Promise.resolve(null);
                    var lib = libs[i++];
                    try {
                        if (lib.libraryType && lib.libraryType !== "user") return next();
                    } catch (e) {}
                    return Promise.resolve()
                        .then(function () {
                            return Zotero.Items.getByLibraryAndKeyAsync(lib.libraryID, key);
                        })
                        .then(function (item) {
                            if (item && !item.deleted) return item;
                            return next();
                        })
                        .catch(function () {
                            return next();
                        });
                }
                return next();
            })
            .catch(function () {
                return null;
            });
    }

    function parentRegularOf(item) {
        return Promise.resolve()
            .then(function () {
                if (isRegular(item)) return item;
                var pid = item && item.parentItemID;
                if (!pid) return null;
                return Zotero.Items.getAsync(pid).then(function (p) {
                    return isRegular(p) ? p : null;
                });
            })
            .catch(function () {
                return null;
            });
    }

    // Read-before-write: tag colours live in the SYNCED setting
    // `syncedSettings.tagColors`, so writing them unconditionally on every
    // 5-minute sync meant pointless DB writes and sync churn.
    function ensureOneTagColor(libraryID, name, color, position) {
        return Promise.resolve()
            .then(function () {
                var cur = null;
                try {
                    cur = Zotero.Tags.getColor(libraryID, name);
                } catch (e) {}
                var curColor = cur && typeof cur === "object" ? cur.color : cur;
                if (curColor === color) return null;
                return Promise.resolve()
                    .then(function () {
                        return Zotero.Tags.setColor(libraryID, name, color, position);
                    })
                    .catch(function () {
                        return Zotero.Tags.setColor(libraryID, name, color);
                    });
            })
            .catch(function () {});
    }

    function ensureTagColors() {
        return Promise.resolve()
            .then(function () {
                var jobs = [];
                Zotero.Libraries.getAll().forEach(function (lib) {
                    if (lib.libraryType && lib.libraryType !== "user") return;
                    jobs.push(ensureOneTagColor(lib.libraryID, TAG_DONE, COLOR_DONE, 0));
                    jobs.push(ensureOneTagColor(lib.libraryID, TAG_TODO, COLOR_TODO, 1));
                });
                return Promise.all(jobs);
            })
            .catch(function () {});
    }

    // ONE-TIME migration. Guarded by a pref so a user-created saved search
    // can never be silently deleted on a later sync.
    function removeUnwantedSearches() {
        return Promise.resolve()
            .then(function () {
                try {
                    if (Zotero.Prefs.get(LEGACY_CLEANUP_PREF, true)) return null;
                } catch (e) {}
                var jobs = [];
                Zotero.Libraries.getAll().forEach(function (lib) {
                    if (lib.libraryType && lib.libraryType !== "user") return;
                    var searches = [];
                    try {
                        searches = Zotero.Searches.getByLibrary(lib.libraryID) || [];
                    } catch (e) {
                        return;
                    }
                    searches.forEach(function (s) {
                        if (UNWANTED.indexOf(s.name || "") === -1) return;
                        jobs.push(
                            Promise.resolve()
                                .then(function () {
                                    log("removing legacy saved search: " + s.name);
                                    return Zotero.Searches.erase(s.id);
                                })
                                .catch(function () {}),
                        );
                    });
                });
                return Promise.all(jobs).then(function () {
                    setPref(LEGACY_CLEANUP_PREF, true);
                });
            })
            .catch(function () {});
    }

    // markPendingFailed 把当前这一轮待办全部标成「翻译失败」。
    //
    // 为什么需要它：后端不可用（网关没起、或起了但没就绪）时，drainQueue 以前只是
    // 记一行日志就返回。用户点「翻译选中条目」看到的是**什么都没发生** —— 没有报错、
    // 没有标签、没有进度窗。而这些条目在 Zotero 里其实可辨认：它们还没有双语件。
    // 给它们打上失败标签，用户至少知道该重试，而不是以为插件坏了。
    //
    // 只标 _pending 里的（= 这一轮真正在等的），不动别的条目。
    function markPendingFailed(reason) {
        var ids = Object.keys(_pending || {});
        log("auto-translate: backend unavailable (" + reason + "), marking " +
            ids.length + " item(s)");
        return ids.reduce(function (chain, id) {
            return chain.then(function () {
                var n = parseInt(id, 10);
                if (!n) return null;
                return Zotero.Items.getAsync(n).then(function (item) {
                    return setStatusTag(item, "failed");
                });
            }).catch(function () {
                return null;
            });
        }, Promise.resolve());
    }

    function setStatusTag(item, mode) {
        return Promise.resolve()
            .then(function () {
                if (!isRegular(item)) return false;
                var changed = false;
                var hasDone = !!item.hasTag(TAG_DONE);
                var hasTodo = !!item.hasTag(TAG_TODO);
                var hasFailed = !!item.hasTag(TAG_FAILED);
                if (mode === "done") {
                    if (hasTodo) {
                        item.removeTag(TAG_TODO);
                        changed = true;
                    }
                    // 也必须清掉「翻译失败」：失败后重试成功的条目，否则会同时
                    // 带着「双语已译」和「翻译失败」—— 两个互相矛盾的结论并存，
                    // 而用户按「翻译失败」筛选时看到的是已经译好的文献。
                    if (hasFailed) {
                        item.removeTag(TAG_FAILED);
                        changed = true;
                    }
                    if (!hasDone) {
                        item.addTag(TAG_DONE);
                        changed = true;
                    }
                } else if (mode === "todo") {
                    if (hasDone) {
                        item.removeTag(TAG_DONE);
                        changed = true;
                    }
                    if (hasFailed) {
                        item.removeTag(TAG_FAILED);
                        changed = true;
                    }
                    if (!hasTodo) {
                        item.addTag(TAG_TODO);
                        changed = true;
                    }
                } else if (mode === "failed") {
                    // 失败是「需要人看一眼」的状态，和 待翻译/已译 互斥 —— 否则一篇
                    // 翻车了的文献会同时带着「双语已译」和「翻译失败」，两种结论并存。
                    if (hasDone) {
                        item.removeTag(TAG_DONE);
                        changed = true;
                    }
                    if (!hasFailed) {
                        item.addTag(TAG_FAILED);
                        changed = true;
                    }
                } else {
                    if (hasDone) {
                        item.removeTag(TAG_DONE);
                        changed = true;
                    }
                    if (hasTodo) {
                        item.removeTag(TAG_TODO);
                        changed = true;
                    }
                }
                if (changed) {
                    return Promise.resolve(item.saveTx()).then(function () {
                        return true;
                    });
                }
                return false;
            })
            .catch(function () {
                return false;
            });
    }

    function parentHasDualAtt(parent) {
        return Promise.resolve()
            .then(function () {
                if (!parent) return false;
                var attIds = parent.getAttachments() || [];
                var i = 0;
                function next() {
                    if (i >= attIds.length) return Promise.resolve(false);
                    var id = attIds[i++];
                    return Promise.resolve()
                        .then(function () {
                            return Zotero.Items.getAsync(id);
                        })
                        .then(function (att) {
                            if (!att) return next();
                            // Zotero.Item has NO `attachmentTitle` property — that is a
                            // getter on the <attachment-row> element only. The old code
                            // read `undefined` here, so the title check never matched
                            // and only the path check below did any work.
                            var title = "";
                            try {
                                title = String(att.getField("title") || "").toLowerCase();
                            } catch (e) {}
                            if (title.indexOf("双语") !== -1 || title.indexOf("dual") !== -1) {
                                return true;
                            }
                            var path = "";
                            try {
                                path = att.getFilePath() || att.attachmentFilename || "";
                            } catch (e) {}
                            if (path && isDualFilename(basename(path))) return true;
                            return next();
                        })
                        .catch(function () {
                            return next();
                        });
                }
                return next();
            })
            .catch(function () {
                return false;
            });
    }

    // Zotero.Prefs.get(pref, global): with global=TRUE the name is used AS-IS,
    // otherwise "extensions.zotero." is prepended. Passing true therefore looked
    // for a bare `dualsync.*` pref that does not exist, so EVERY pref here
    // silently fell back to its code default (that is why autoTranslateMax
    // always logged as 3 no matter what prefs.js said). Omit the flag.
    function getPref(name, dflt) {
        try {
            var v = Zotero.Prefs.get(name);
            return v === undefined || v === null ? dflt : v;
        } catch (e) {
            return dflt;
        }
    }

    function setPref(name, value) {
        try {
            Zotero.Prefs.set(name, value);
        } catch (e) {
            log("setPref(" + name + ") failed: " + e);
        }
    }

    // Last-modified time of a path in ms, or null if it cannot be determined.
    // Used to skip storage folders that have not changed since the last pass.
    function statMtime(path) {
        return Promise.resolve()
            .then(function () {
                if (typeof IOUtils !== "undefined" && IOUtils.stat) return IOUtils.stat(path);
                return null;
            })
            .then(function (st) {
                return st && st.lastModified ? st.lastModified : null;
            })
            .catch(function () {
                return null;
            });
    }

    // Attach a discovered dual to its parent item.
    // The old code fell back to importFromFile() on any link failure, which
    // silently COPIED the PDF into a new storage folder — with a broken
    // duplicate check and a 5-minute timer that could pile up copies. Now the
    // mode is an explicit choice and a failure is reported, never papered over.
    function linkDual(dualPath, parent, title) {
        return Promise.resolve()
            .then(function () {
                var file = toFile(dualPath);
                if (!file) throw new Error("bad path: " + dualPath);
                var mode = String(getPref(LINK_MODE_PREF, "linked")).toLowerCase();

                if (mode === "managed") {
                    return Zotero.Attachments.importFromFile({
                        file: file,
                        parentItemID: parent.id,
                    }).then(function (itemID) {
                        if (!itemID) throw new Error("importFromFile returned nothing");
                        return Zotero.Items.getAsync(itemID)
                            .then(function (att) {
                                if (!att) return itemID;
                                try {
                                    att.setField("title", title);
                                } catch (e) {}
                                return att.saveTx().then(function () {
                                    return itemID;
                                });
                            })
                            .then(function () {
                                if (getPref(MANAGED_DELETE_SOURCE_PREF, false)) {
                                    return IOUtils.remove(dualPath).catch(function (e) {
                                        log("could not delete source after import: " + e);
                                    });
                                }
                                return itemID;
                            });
                    });
                }

                return Zotero.Attachments.linkFromFile({
                    file: file,
                    parentItemID: parent.id,
                    title: title,
                }).catch(function (e) {
                    log("linkFromFile FAILED (refusing to silently import): " + e);
                    throw e;
                });
            });
    }

    // ---------------------------------------------------------------- exclusion
    //
    // Which collections must not be translated.  The list lives in the shared
    // zotero-tools config so the batch script, this plugin and LitFlow all read
    // one source of truth:
    //
    //   config.json  -> "exclude_collections" -> { "exclude": [{"key": "..."}] }
    //
    // Matching is by collection KEY (unique, survives renames), and a collection
    // that is excluded also excludes everything nested under it — walked here via
    // parentID, so no extra data is needed.
    //
    // ANY failure (config missing, path missing, JSON broken, file unreadable)
    // means "exclude nothing": the item is translated as before.  Skipping a
    // paper the user wanted is worse than translating one they did not.
    var EXCLUDE_PREF = "dualsync.zoteroToolsConfig";
    var DEFAULT_CONFIG = "D:\\zotero-tools\\config.json";
    var _excludeCache = null;   // { mtime, keys, names }

    // `Zotero.File.getContents` is synchronous and marked @deprecated in favour of
    // getContentsAsync, but it is still defined and still used by Zotero's own
    // cite.js / style.js (checked in the installed Zotero 10.0.5 bundle).  Using it
    // keeps this helper synchronous at the call site; if a future Zotero removes
    // it, the try/catch below degrades to "exclude nothing" rather than throwing
    // inside the drain loop.
    // Where the collection exclusion list lives, for telling the user which file
    // to edit when their request was refused. Read-only and failure-tolerant: a
    // missing or broken config returns the raw pref value so the message can
    // still name something, rather than itself throwing inside a dialog path.
    function excludeCollectionsPath() {
        var cfgPath = String(getPref(EXCLUDE_PREF, DEFAULT_CONFIG) || DEFAULT_CONFIG);
        try {
            var cfgFile = toFile(cfgPath);
            if (cfgFile && cfgFile.exists()) {
                var cfg = JSON.parse(Zotero.File.getContents(cfgFile));
                if (cfg && cfg.exclude_collections) return String(cfg.exclude_collections);
            }
        } catch (e) {}
        return "(在 " + cfgPath + " 的 exclude_collections 中指定)";
    }

    function loadExclusions() {
        try {
            var cfgFile = toFile(String(getPref(EXCLUDE_PREF, DEFAULT_CONFIG) || DEFAULT_CONFIG));
            if (!cfgFile || !cfgFile.exists()) return null;
            var cfg = JSON.parse(Zotero.File.getContents(cfgFile));
            var listPath = cfg.exclude_collections;
            if (!listPath) return null;
            var listFile = toFile(String(listPath));
            if (!listFile.exists()) return null;
            var mtime = listFile.lastModifiedTime;
            if (_excludeCache && _excludeCache.mtime === mtime) return _excludeCache;
            var data = JSON.parse(Zotero.File.getContents(listFile));
            var keys = {}, names = {};
            (data.exclude || []).forEach(function (e) {
                if (e && e.key) {
                    keys[e.key] = 1;
                    names[e.key] = e.name || e.key;
                }
            });
            _excludeCache = { mtime: mtime, keys: keys, names: names };
            return _excludeCache;
        } catch (e) {
            log("exclude: 读取排除配置失败，本次不排除任何分类 (" + e + ")");
            return null;
        }
    }

    /** Returns the matching collection key, or null when the item is allowed. */
    function excludedCollectionOf(item) {
        var cfg = loadExclusions();
        if (!cfg || !cfg.keys) return null;
        var ids = [];
        try {
            ids = item.getCollections() || [];
        } catch (e) {
            return null;   // cannot tell -> do not exclude
        }
        for (var i = 0; i < ids.length; i++) {
            var col = Zotero.Collections.get(ids[i]);
            var guard = 0;
            while (col && guard++ < 64) {
                if (cfg.keys[col.key]) return col.key;
                col = col.parentID ? Zotero.Collections.get(col.parentID) : null;
            }
        }
        return null;
    }

    // ---------------------------------------------------------------- translate

    function autoTranslateEnabled() {
        return getPref(AUTO_TRANSLATE_PREF, true) === true;
    }

    function manualBypassesCollectionExclude() {
        return getPref(MANUAL_BYPASS_PREF, true) === true;
    }

    // `item.hasTag` is the obvious call, but it is reached from a drain that
    // must not die on a half-loaded item, and the failure direction matters:
    // if we cannot read the tags we translate, because a missing guard is
    // better than a silently skipped paper.
    function hasTagSafely(item, tag) {
        try {
            return !!item.hasTag(tag);
        } catch (e) {
            log("auto-translate: could not read tags (" + e + ") - translating anyway");
            return false;
        }
    }

    // --- backend selection ------------------------------------------------
    // "litflow" talks to the resident service, which owns the queue. "script"
    // is the original per-item python spawn. The default is litflow WITH
    // fallback: a machine where LitFlow is not running must still translate,
    // and a plugin that silently stops working because a service is down is
    // worse than one that degrades to the old path.
    // 只剩一个后端了（脚本后端已删）。保留这个函数是因为 prefs 里可能还留着旧值
    // "script"：读到它也只当 litflow —— 否则升级后那个旧值会让插件找不到任何可用
    // 后端，表现为彻底静默不翻译，而用户看不出是配置残留。
    function configuredBackend() {
        return "litflow";
    }

    function litflowBase() {
        return String(getPref(LITFLOW_URL_PREF, DEFAULT_LITFLOW_URL) || "")
            .replace(/\/+$/, "");
    }

    // Shared request options for every LitFlow call.
    //
    // `errorDelayMax: 0` is load-bearing, not defensive. Zotero.HTTP.request
    // retries 429/5xx AUTOMATICALLY with delays of 2.5s, 5s, 10s, ... 300s and
    // a default ceiling of ONE HOUR (xpcom/http.js: _errorDelayIntervals,
    // _errorDelayMax). LitFlow answers 503 when every candidate is cooling
    // precisely so the caller knows to come back later — but left to the
    // default, Zotero would silently retry that for an hour and the plugin
    // would look hung rather than reporting what happened. We do our own
    // waiting (poll interval, per-job timeout) and want failures now.
    //
    // `noRetryOnThrottle: true` covers 429/503 that carry Retry-After, which
    // our gateway does send.
    function litflowRequestOptions(extra) {
        var options = {
            errorDelayMax: 0,
            noRetryOnThrottle: true,
            responseType: "json",
        };
        if (extra) {
            Object.keys(extra).forEach(function (k) {
                options[k] = extra[k];
            });
        }
        return options;
    }

    // A cheap liveness probe, used once per drain rather than per item.
    // Resolves to { up, ready }: `up` means the service is listening (it binds
    // BEFORE fetching its model catalogue, precisely so this can answer early),
    // `ready` means translation can actually start.
    function litflowProbe() {
        return Zotero.HTTP.request(
            "GET",
            litflowBase() + "/health",
            litflowRequestOptions({ timeout: 4000 }),
        ).then(function (xhr) {
            var body = xhr.response || {};
            return { up: true, ready: body.ready !== false };
        }).catch(function () {
            return { up: false, ready: false };
        });
    }

    // The backend actually in use for this drain, decided once so a service
    // that dies mid-queue is reported rather than re-probed per item.
    //
    // 这里曾经有一个「脚本后端」回退：LitFlow 不答时改跑
    // D:\zotero-tools\pdf2zh\batch_translate_live.py。那是这个插件当主力的年代
    // 留下的第二套实现，现在它是**死路**：
    //
    //   · 那个脚本的 llm_base_url 指向 http://127.0.0.1:8787/v1，也就是已经被
    //     本网关取代、早已退役的旧面板；8787 上什么都没有；
    //   · 它绕过整个网关 —— 没有账号池、没有轮换、没有限流冷却、没有队列、
    //     没有术语表。就算它能跑，产出的可靠性也和网关不是一回事；
    //   · 失败时**只写一行日志**，用户在 Zotero 里看到的是「点了没反应」。
    //
    // 一条永远跑不通、又只把错误吞进日志的回退，比一个明确的报错更糟：它让
    // 故障看起来像沉默。所以现在只有一条路径，LitFlow 不在就如实说。
    function resolveBackend() {
        var base = litflowBase();
        var waited = 0;
        var stepMs = 1500;
        var budgetMs = 60000;

        // A service that is up but still loading its catalogue is USABLE, just
        // not yet — so wait rather than giving up. Giving up here would mean the
        // first translation after every restart fails for no reason, which is
        // exactly the kind of "it works on the second try" flakiness this
        // project keeps trying to remove.
        function waitReady() {
            return litflowProbe().then(function (probe) {
                if (!probe.up) {
                    log("auto-translate: LitFlow not answering at " + base);
                    return { kind: "unavailable", reason: "litflow-down", base: base };
                }
                if (probe.ready) return { kind: "litflow", base: base };
                if (waited >= budgetMs) {
                    log(
                        "auto-translate: LitFlow is up but still not ready after " +
                            Math.round(waited / 1000) + "s",
                    );
                    return { kind: "unavailable", reason: "litflow-not-ready", base: base };
                }
                waited += stepMs;
                return Zotero.Promise.delay(stepMs).then(waitReady);
            });
        }
        return waitReady();
    }

    // --- the LitFlow backend ----------------------------------------------
    // POST one PDF, then poll its job. The plugin holds no queue of its own:
    // the service does, which is why there is nothing here about serialising
    // runs or guarding against a second instance.
    function litflowSubmit(backend, sourcePath, key) {
        return Zotero.HTTP.request(
            "POST",
            backend.base + "/translate",
            litflowRequestOptions({
                headers: { "Content-Type": "application/json" },
                body: JSON.stringify({ source: sourcePath }),
                timeout: 30000,
            }),
        ).then(function (xhr) {
            var job = xhr.response || {};
            if (!job.id) throw new Error("no job id in the response");
            log("litflow: job " + job.id + " accepted (" + job.state + ") for " + key);
            if (job.state === "skipped") return true;
            return litflowWait(backend, job.id, key);
        }).catch(function (e) {
            log("litflow: " + key + " submit FAILED: " + e);
            return false;
        });
    }

    function litflowWait(backend, jobId, key) {
        var pollMs = parseInt(getPref(LITFLOW_POLL_PREF, 2000), 10);
        if (isNaN(pollMs) || pollMs < 250) pollMs = 2000;
        var timeoutMs = parseInt(getPref(LITFLOW_TIMEOUT_PREF, 7200000), 10);
        if (isNaN(timeoutMs) || timeoutMs <= 0) timeoutMs = 7200000;
        var started = Date.now();
        var lastNote = 0;

        function step() {
            return Zotero.HTTP.request(
                "GET",
                backend.base + "/translate/" + jobId,
                litflowRequestOptions({ timeout: 30000 }),
            ).then(function (xhr) {
                var job = xhr.response || {};
                var elapsed = Date.now() - started;
                if (job.state === "done" || job.state === "skipped") {
                    log("litflow: " + key + " " + job.state + " in " +
                        Math.round(elapsed / 1000) + "s");
                    return true;
                }
                if (job.state === "failed" || job.state === "cancelled") {
                    // The silent failure is the one worth shouting about: pdf2zh
                    // exited 0 and wrote nothing, so the PDF would be quietly
                    // missing paragraphs.
                    log("litflow: " + key + " " + job.state +
                        (job.produced_nothing ? " (PRODUCED NO OUTPUT)" : "") +
                        ": " + (job.error || "no detail"));
                    return false;
                }
                if (elapsed > timeoutMs) {
                    log("litflow: " + key + " still running after " +
                        Math.round(elapsed / 1000) + "s - giving up on this item");
                    return false;
                }
                // Progress is coarse by design: pdf2zh suppresses its stage bars
                // when its output is piped, so the service reports elapsed time
                // rather than inventing a percentage. Echo that, at most every
                // 30s, so the log shows liveness without becoming a wall.
                if (elapsed - lastNote > 30000) {
                    lastNote = elapsed;
                    log("litflow: " + key + " " + (job.liveness || job.state));
                }
                return Zotero.Promise.delay(pollMs).then(step);
            }).catch(function (e) {
                // A 404 is NOT a missed beat: it means the service no longer knows
                // this job at all -- typically the gateway restarted, and its queue
                // lives in memory. Left as a retry it would poll for the full timeout
                // (two hours by default) on a job that can never appear, and the
                // plugin would look hung rather than report anything.
                var status = e && (e.status || (e.xhr && e.xhr.status));
                if (status === 404 || status === 410) {
                    log("litflow: " + key + " job " + jobId + " is gone (HTTP " +
                        status + ") -- the gateway restarted; submit it again");
                    return false;
                }
                // Anything else can be transient - the service may be busy
                // translating. Treat it as a missed beat and keep polling until the
                // overall timeout.
                if (Date.now() - started > timeoutMs) {
                    log("litflow: " + key + " poll failed and timed out: " + e);
                    return false;
                }
                return Zotero.Promise.delay(pollMs).then(step);
            });
        }
        return step();
    }


    // the attachment whose storage folder holds the source PDF
    function findOriginalAttachment(parent) {
        return Promise.resolve()
            .then(function () {
                var atts = parent.getAttachments() || [];
                var i = 0;
                function next() {
                    if (i >= atts.length) return null;
                    var aid = atts[i++];
                    return Zotero.Items.getAsync(aid)
                        .then(function (a) {
                            if (!a) return next();
                            var path = "";
                            try {
                                path = a.getFilePath() || "";
                            } catch (e) {}
                            var fn = basename(path);
                            if (fn && /\.pdf$/i.test(fn) && isOriginalPdf(fn)) return a;
                            return next();
                        })
                        .catch(function () {
                            return next();
                        });
                }
                return next();
            })
            .catch(function () {
                return null;
            });
    }

    // 只有一条路径：LitFlow。第二个「脚本后端」已经删掉（见 resolveBackend 的说明）——
    // 它指向的旧面板早已退役，留着只会把失败吞进日志。
    function runTranslation(backend, key, sourcePath) {
        return litflowSubmit(backend, sourcePath, key);
    }

    // BUG in 1.0.7: every sync called autoTranslatePending(), and each call
    // started its OWN chain, so two different items could be translated at the
    // same time (and two 1.8 GB babeldoc processes would fight for the machine).
    // Now there is exactly ONE drain loop; syncs only add to a pending set.
    function autoTranslatePending(todoIds, force) {
        if (!autoTranslateEnabled() && !force) return;
        var added = 0;
        var revived = 0;
        Object.keys(todoIds).forEach(function (k) {
            // `_attempted` is a session guard against retry LOOPS, and it is
            // never reset. Letting it veto an explicit request meant the menu
            // item silently did nothing for any item the automatic pass had
            // already touched — which is exactly what a user reports as "I
            // clicked translate and nothing happened". A human asking is not a
            // loop, so force clears the marker.
            if (force && _attempted[k]) {
                delete _attempted[k];
                revived++;
            }
            if (!_pending[k] && !_attempted[k]) {
                _pending[k] = 1;
                added++;
            }
        });
        if (revived) {
            log("auto-translate: re-queued " + revived +
                " item(s) that a previous pass had already attempted");
        }
        if (_draining) {
            if (added) log("auto-translate: queued " + added + " more (a job is already running)");
            return;
        }
        if (!added && !force) return;
        _draining = true;
        drainQueue(force);
    }

    function drainQueue(force) {
        // The backend is resolved ONCE per drain, not per item: probing the
        // service for every PDF would add a round trip each time, and a service
        // that dies mid-queue should be reported rather than silently retried.
        resolveBackend().then(function (backend) {
            if (backend.kind !== "litflow") {
                // 后端不可用 —— 这一次刷队列什么也做不了。标记待办条目并复位，
                // 否则 _draining 会一直卡在 true，之后所有翻译请求都被静默吞掉。
                markPendingFailed(backend.reason);
                _draining = false;
                return;
            }
            drainWith(backend, force);
        }).catch(function (e) {
            log("auto-translate: backend resolution failed: " + e);
            markPendingFailed("backend-error");
            _draining = false;
        });
    }

    function drainWith(backend, force) {
        var budget = force ? Infinity : parseInt(getPref(TRANSLATE_MAX_PREF, 3), 10);
        if (!force && (isNaN(budget) || budget < 0)) budget = 3;

        var waiting = Object.keys(_pending).filter(function (id) {
            return !_attempted[id];
        });
        if (!waiting.length) {
            // Log it. This branch used to return in silence, and that silence
            // is what made "I clicked translate and nothing happened"
            // undiagnosable: the queue was empty because every item had already
            // been attempted, and nothing said so.
            var queued = Object.keys(_pending).length;
            log(
                "auto-translate: nothing to do - " + queued +
                    " pending but all already attempted this session" +
                    (force ? "" : ", and this is an automatic pass"),
            );
            _draining = false;
            return;
        }
        if (_translateStarted >= budget) {
            log(
                "auto-translate: session cap " + budget + " reached; " + waiting.length +
                    " item(s) still pending (menu: Dual Sync > translate selected)",
            );
            _draining = false;
            return;
        }

        var id = parseInt(waiting[0], 10);
        _attempted[id] = 1;
        delete _pending[id];

        Zotero.Items.getAsync(id)
            .then(function (item) {
                if (!item || item.deleted || !isRegular(item)) {
                    // log the skip: in 1.0.8 the queue drained silently and it
                    // looked like the feature had simply not run
                    log("auto-translate: skip item " + id + " (missing, trashed or not regular)");
                    return null;
                }
                // --- exclusions -------------------------------------------------
                // Two kinds, and they deserve different treatment because they
                // say different things:
                //
                //   * a TAG ("不翻译") is a per-item, deliberate mark — "not
                //     this paper". It is respected even for an explicit request.
                //   * a COLLECTION is a bulk grouping. The batch script used to
                //     enforce the tag, so with the LitFlow backend this plugin
                //     became the only place that can — hence the tag check here.
                //
                // A manual menu action bypasses the COLLECTION rule, because
                // clicking one item is an explicit, more recent instruction than
                // a grouping, and this file's own principle is that skipping a
                // paper the user wanted is worse than translating one they did
                // not. Automatic passes still honour it, so the bulk protection
                // is untouched.
                var excludedBy = excludedCollectionOf(item);
                if (excludedBy) {
                    if (force && manualBypassesCollectionExclude()) {
                        log("auto-translate: item " + id + " is in an excluded " +
                            "collection (" + excludedBy + ") but this is an explicit " +
                            "request - translating anyway");
                    } else {
                        _skippedForExclusion[id] = "collection " + excludedBy;
                        log("auto-translate: skip item " + id +
                            " (collection excluded: " + excludedBy + ")");
                        return null;
                    }
                }

                var excludedTag = excludeTagName();
                if (excludedTag && hasTagSafely(item, excludedTag)) {
                    _skippedForExclusion[id] = "tag " + excludedTag;
                    log("auto-translate: skip item " + id +
                        " (tagged '" + excludedTag + "')");
                    return null;
                }
                return findOriginalAttachment(item).then(function (att) {
                    if (!att || !att.key) {
                        log("auto-translate: skip item " + id + " (no usable PDF attachment)");
                        return null;
                    }
                    var key = att.key;
                    // LitFlow is addressed by FILE PATH, where the script was
                    // addressed by storage key and looked the path up itself.
                    // Captured here because the attachment object is in hand.
                    var sourcePath = "";
                    try {
                        sourcePath = att.getFilePath() || "";
                    } catch (e) {}
                    if (backend.kind === "litflow" && !sourcePath) {
                        log("auto-translate: skip item " + id +
                            " (LitFlow needs a file path and this attachment has none)");
                        return null;
                    }
                    if (_translating[key]) return null;
                    _translating[key] = 1;
                    _translateStarted++;
                    log(
                        "auto-translate: START " + key +
                            " via " + backend.kind +
                            " (" + _translateStarted +
                            (budget === Infinity ? "" : "/" + budget) + ") " +
                            itemTitle(item).slice(0, 46),
                    );
                    return runTranslation(backend, key, sourcePath).then(function (ok) {
                        delete _translating[key];
                        log("auto-translate: DONE " + key + " ok=" + ok + " - syncing");
                        // the dual is on disk now; attach + tag it immediately
                        return syncOnce({ silent: true });
                    });
                });
            })
            .catch(function (e) {
                log("auto-translate: item " + id + " error: " + e);
            })
            .then(function () {
                drainWith(backend, force); // strictly one job at a time
            });
    }

    // menu action: translate the items selected in the item pane
    function translateSelected() {
        // No up-front translatePaths() gate here any more: with the LitFlow
        // backend there is no Python script to check, and refusing before the
        // queue is even consulted would block a setup that works. The backend
        // resolution inside drainQueue reports the real reason if there is one.
        var selected = [];
        try {
            selected = Zotero.getMainWindow().ZoteroPane.getSelectedItems() || [];
        } catch (e) {}
        var todo = {};
        selected.forEach(function (it) {
            if (isRegular(it)) todo[it.id] = 1;
        });
        var n = Object.keys(todo).length;
        if (!n) {
            Zotero.alert(null, "Dual Sync", "请先在条目列表中选中要翻译的条目。");
            return;
        }
        log("translate selected: " + n + " item(s)");

        // Watch what actually happened to the items the user asked about, and
        // say so if none of them could start. Every silent refusal in this
        // plugin has cost a round of "it does nothing" — an explicit request
        // that cannot be honoured must answer.
        var wanted = Object.keys(todo).map(Number);
        _skippedForExclusion = {};
        setTimeout(function () {
            var started = wanted.filter(function (w) {
                return !_skippedForExclusion[w] && !_pending[w];
            });
            if (started.length) return;   // something is running; stay quiet
            var reasons = wanted
                .filter(function (w) { return _skippedForExclusion[w]; })
                .map(function (w) { return _skippedForExclusion[w]; });
            if (!reasons.length) return;  // no exclusion involved
            var uniq = reasons.filter(function (r, i) { return reasons.indexOf(r) === i; });
            Zotero.alert(
                null, "Dual Sync",
                "选中的 " + wanted.length + " 个条目没有开始翻译：\n\n" +
                uniq.map(function (r) { return "  · " + r; }).join("\n") +
                "\n\n分类排除的名单在：\n" + excludeCollectionsPath() +
                "\n带「" + (excludeTagName() || "不翻译") + "」标签的条目请先移除标签。",
            );
        }, 4000);

        autoTranslatePending(todo, true);
    }

    // ------------------------------------------------------------- db backup

    // Apply the two Zotero backup knobs from our own prefs, so retention and
    // interval live next to everything else. Only ever RAISES retention.
    function applyBackupPrefs() {
        if (getPref("dualsync.manageBackupPrefs", true) !== true) return;
        try {
            // retention and interval are in Zotero's own units (count / minutes)
            var wantCopies = parseInt(getPref(LOCAL_COPIES_PREF, 24), 10);
            if (isNaN(wantCopies) || wantCopies < 0) wantCopies = 24;
            if (wantCopies > 24) wantCopies = 24;   // Zotero caps it there
            var haveCopies = Zotero.Prefs.get("backup.numBackups");
            if (wantCopies !== haveCopies) {
                Zotero.Prefs.set("backup.numBackups", wantCopies);
                log("backup: numBackups " + haveCopies + " -> " + wantCopies);
            }

            var wantMin = parseInt(getPref(BACKUP_MINUTES_PREF, 120), 10);
            if (isNaN(wantMin) || wantMin < 1) wantMin = 120;
            var haveMin = Zotero.Prefs.get("backup.interval");
            if (wantMin !== haveMin) {
                Zotero.Prefs.set("backup.interval", wantMin);
                log("backup: interval " + haveMin + " -> " + wantMin + " min");
            }
        } catch (e) {
            log("backup: could not apply prefs: " + e);
        }
    }

    function backupStatus(extra) {
        var info = {
            writtenBy: "dualsync",
            lastAttempt: new Date().toISOString(),
            ok: false,
            error: null,
            numBackups: null,
            intervalMin: null,
            newestBak: null,
            newestBakBytes: null,
        };
        try {
            info.numBackups = Zotero.Prefs.get("backup.numBackups");
            info.intervalMin = Zotero.Prefs.get("backup.interval");
        } catch (e) {}
        if (extra) for (var k in extra) info[k] = extra[k];
        return info;
    }

    function findNewestBak() {
        return Promise.resolve()
            .then(function () {
                var dir = Zotero.DataDirectory.dir;
                return listChildren(dir);
            })
            .then(function (entries) {
                var baks = (entries || []).filter(function (p) {
                    return /zotero\.sqlite(\.\d+)?\.bak$/i.test(basename(p));
                });
                if (!baks.length) return null;
                return Promise.all(
                    baks.map(function (p) {
                        return IOUtils.stat(p)
                            .then(function (st) {
                                return { path: p, mtime: st.lastModified || 0, size: st.size || 0 };
                            })
                            .catch(function () {
                                return { path: p, mtime: 0, size: 0 };
                            });
                    }),
                ).then(function (all) {
                    all.sort(function (a, b) {
                        return b.mtime - a.mtime;
                    });
                    return all[0];
                });
            })
            .catch(function () {
                return null;
            });
    }

    // The plugin and the uploader script both write this file, so MERGE rather
    // than replace. (Bug in 1.0.13: the plugin wrote a fresh object without the
    // uploader's lastUploadAt, which made the health check cry
    // "never uploaded to the cloud" right after a successful upload.)
    function writeStatusFile(info) {
        return readStatus()
            .then(function (prev) {
                var merged = {};
                if (prev && typeof prev === "object") {
                    for (var k in prev) if (k in prev) merged[k] = prev[k];
                }
                for (var k2 in info) if (k2 in info) merged[k2] = info[k2];
                return IOUtils.writeUTF8(STATUS_FILE, JSON.stringify(merged, null, 2));
            })
            .catch(function (e) {
                log("backup: could not write status file: " + e);
            });
    }

    function readStatus() {
        return Promise.resolve()
            .then(function () {
                var f = toFile(STATUS_FILE);
                if (!f || !f.exists()) return null;
                return IOUtils.readUTF8(STATUS_FILE).then(function (txt) {
                    try {
                        return JSON.parse(txt);
                    } catch (e) {
                        return null;
                    }
                });
            })
            .catch(function () {
                return null;
            });
    }

    // Run the uploader. It reads its OWN credential (DPAPI), so the plugin never
    // touches the WebDAV password.
    function runUploader(force) {
        var py = String(getPref(PYTHON_PREF, DEFAULT_PYTHON) || "");
        var script = String(getPref(BACKUP_SCRIPT_PREF, DEFAULT_BACKUP_SCRIPT) || "");
        var pf = py ? toFile(py) : null;
        var sf = script ? toFile(script) : null;
        if (!pf || !pf.exists()) return Promise.resolve({ ok: false, error: "python not found: " + py });
        if (!sf || !sf.exists()) return Promise.resolve({ ok: false, error: "backup script not found: " + script });
        // ASCII arg on purpose: non-ASCII command-line arguments get mangled by
        // the shell/codepage layer before the script sees them.
        var argv = [sf.path, "--by", "zotero"];
        if (force) argv.push("--force");
        return Zotero.Utilities.Internal.exec(pf, argv).then(
            function () {
                return { ok: true, error: null };
            },
            function (e) {
                return { ok: false, error: String(e) };
            },
        );
    }

    // Warn when the backup CHAIN is unhealthy. Note the distinction: the uploader
    // finding "nothing new to upload" is a SUCCESS, not a failure - so this keys
    // off when the chain last RAN and whether it succeeded, not off whether a
    // file was uploaded.
    function checkBackupHealth() {
        if (getPref("dualsync.checkBackupOnStartup", true) !== true) return;
        var warnHours = parseInt(getPref(WARN_AFTER_HOURS_PREF, 48), 10);
        if (isNaN(warnHours) || warnHours < 1) warnHours = 48;

        readStatus().then(function (st) {
            var problems = backupProblems(st, warnHours);

            if (!problems.length) {
                log(
                    "backup: health OK (last run " + (st.lastRunAt || "?") +
                        ", last upload " + (st.lastUploadAt || "?") + ")",
                );
                return;
            }
            log("backup: HEALTH PROBLEM -- " + problems.join(" / "));
            if (_backupWarned) return;
            _backupWarned = true;
            var msg = "Zotero 数据库备份有问题：\n\n• " + problems.join("\n• ") +
                "\n\n检查：D:\\zotero-tools\\logs\\backup.log\n" +
                "手动补一次：工具 → Dual Sync：立即备份数据库";
            if (String(getPref(WARN_MODE_PREF, "alert")) === "log") {
                log("backup: (warn mode = log, not showing a dialog)");
                return;
            }
            try {
                Zotero.alert(null, "Dual Sync — 数据库备份异常", msg);
            } catch (e) {
                log("backup: could not show the warning dialog: " + e);
            }
        });
    }

    function backupNow(reason, silent) {
        log("backup: forced backup requested (" + reason + ")");
        applyBackupPrefs();
        var summary = {};
        return Promise.resolve()
            .then(function () {
                return Zotero.DB.backUpDatabase({ force: true });
            })
            .catch(function (e) {
                log("backup: backUpDatabase failed: " + e);
                summary.snapshotError = String(e);
            })
            .then(function () {
                return findNewestBak();
            })
            .then(function (bak) {
                summary.newestBak = bak ? bak.path : null;
                summary.newestBakBytes = bak ? bak.size : null;
                summary.newestBakTime = bak ? new Date(bak.mtime).toISOString() : null;
                log(
                    "backup: snapshot " +
                        (bak ? basename(bak.path) + " " + (bak.size / 1e6).toFixed(2) + " MB" : "NOT FOUND"),
                );
                // manual click = the user expects it ON THE CLOUD now, not in 6 h
                if (reason === "menu" && getPref(UPLOAD_ON_MANUAL_PREF, true) === true) {
                    log("backup: uploading now ...");
                    return runUploader(true).then(function (r) {
                        summary.uploadOk = r.ok;
                        summary.uploadError = r.error;
                        log("backup: upload " + (r.ok ? "OK" : "FAILED: " + r.error));
                    });
                }
                return null;
            })
            .then(function () {
                var fields = {
                    ok: !summary.snapshotError && summary.uploadOk !== false,
                    trigger: reason,
                    lastRunBy: reason === "menu" ? "Zotero 内手动点击" : reason,
                    newestBak: summary.newestBak,
                    newestBakBytes: summary.newestBakBytes,
                    newestBakTime: summary.newestBakTime,
                    uploadError: summary.uploadError || summary.snapshotError || null,
                };
                if (summary.uploadOk === true) {
                    // record it here too, so the field is right even if the
                    // uploader's own write is lost
                    fields.lastUploadAt = new Date().toISOString();
                    fields.lastUploadedBy = "dualsync:" + reason;
                }
                return writeStatusFile(backupStatus(fields));
            })
            .then(function () {
                if (reason !== "menu" || silent) return summary;
                var lines = [];
                lines.push(
                    summary.newestBak
                        ? "本地快照：" + basename(summary.newestBak) + "  " +
                          (summary.newestBakBytes / 1e6).toFixed(2) + " MB"
                        : "本地快照：未找到（backUpDatabase 可能失败）",
                );
                if (summary.snapshotError) lines.push("快照错误：" + summary.snapshotError);
                if (summary.uploadOk === true) lines.push("云端上传：成功");
                else if (summary.uploadOk === false) lines.push("云端上传：失败 — " + summary.uploadError);
                else lines.push("云端上传：未执行（uploadOnManualBackup=false）");
                try {
                    Zotero.alert(null, "Dual Sync — 数据库备份", lines.join("\n"));
                } catch (e) {}
                return summary;
            });
    }

    // ---------------------------------------------------- status rendering

    function pad2(n) {
        return (n < 10 ? "0" : "") + n;
    }

    // The status file mixes UTC ("...Z", written by the plugin) and naive local
    // time (written by the Python script). Date() parses both correctly; this
    // only formats for humans.
    function fmtTime(iso) {
        if (!iso) return "未知";
        var d = new Date(iso);
        if (isNaN(d.getTime())) return String(iso);
        return (
            d.getFullYear() + "-" + pad2(d.getMonth() + 1) + "-" + pad2(d.getDate()) +
            " " + pad2(d.getHours()) + ":" + pad2(d.getMinutes())
        );
    }

    function agoText(iso) {
        if (!iso) return "";
        var d = new Date(iso);
        if (isNaN(d.getTime())) return "";
        var mins = Math.round((Date.now() - d.getTime()) / 60000);
        if (mins < 1) return "刚刚";
        if (mins < 60) return mins + " 分钟前";
        var hours = mins / 60;
        if (hours < 48) return hours.toFixed(1) + " 小时前";
        return (hours / 24).toFixed(1) + " 天前";
    }

    function fmtMB(bytes) {
        if (!bytes) return "?";
        return (bytes / 1e6).toFixed(2) + " MB";
    }

    // Single source of truth for "is the backup chain healthy", shared by the
    // startup warning and the status dialog so they can never disagree.
    function backupProblems(st, warnHours) {
        var problems = [];
        if (!st) {
            problems.push("还没有备份状态文件 —— 备份链从未成功运行过");
            return problems;
        }
        if (st.lastRunOk === false) {
            problems.push("上次上传失败：" + (st.uploadError || "原因未知"));
        }
        if (st.lastRunAt) {
            var ageH = (Date.now() - new Date(st.lastRunAt).getTime()) / 3.6e6;
            if (ageH > warnHours) {
                problems.push(
                    "备份链已 " + ageH.toFixed(0) + " 小时没有运行（阈值 " + warnHours +
                        " 小时）—— 计划任务可能被停用，或机器长期关机",
                );
            }
        }
        if (!st.lastUploadAt) {
            problems.push("从未成功上传过快照到云端");
        }
        return problems;
    }

    function renderBackupStatus(st, problems) {
        var L = [];
        var ok = problems.length === 0;

        L.push(ok ? "数据库备份：正常 ✓" : "数据库备份：有问题 ✗");
        L.push("");

        if (!st) {
            L.push("从来没有成功运行过备份链。");
            L.push("请检查 D:\\zotero-tools\\logs\\backup.log，");
            L.push("或点「立即备份数据库」手动试一次。");
            return L.join("\n");
        }

        // ---- local snapshot
        L.push("【本地快照】");
        if (st.newestBak) {
            L.push("  最新：" + basename(st.newestBak) + "（" + fmtMB(st.newestBakBytes) + "）");
            L.push("  时间：" + fmtTime(st.newestBakTime) + "  " + agoText(st.newestBakTime));
        } else {
            L.push("  未找到快照文件");
        }
        L.push("  设置：保留 " + (st.numBackups || "?") + " 份，每 " +
               (st.intervalMin || "?") + " 分钟一次");
        if (st.verify) L.push("  校验：" + st.verify);
        L.push("  位置：" + Zotero.DataDirectory.dir);

        // ---- cloud
        L.push("");
        L.push("【云端备份（夸克网盘）】");
        if (st.lastUploadAt) {
            L.push("  最近上传：" + fmtTime(st.lastUploadAt) + "  " + agoText(st.lastUploadAt));
            L.push("  文件：" + (st.lastUploaded || "?") + "（" + fmtMB(st.lastUploadedBytes) + "）");
        } else {
            L.push("  从未成功上传过");
        }
        if (typeof st.cloudZips === "number") L.push("  云端现有：" + st.cloudZips + " 份");
        L.push("  保留策略：最近 14 天每天一份 + 8 周每周一份");
        if (st.uploadError) L.push("  上次错误：" + st.uploadError);

        // ---- chain
        L.push("");
        L.push("【备份链】");
        L.push("  上次运行：" + fmtTime(st.lastRunAt) + "  " + agoText(st.lastRunAt) +
               "  " + (st.lastRunOk === false ? "（失败）" : "（成功）"));
        if (st.lastRunBy) L.push("  触发：" + st.lastRunBy);

        if (!ok) {
            L.push("");
            L.push("【问题】");
            problems.forEach(function (p) {
                L.push("  • " + p);
            });
            L.push("");
            L.push("【怎么办】");
            L.push("  1. 确认 AList 在运行（127.0.0.1:5244）");
            L.push("  2. 点「立即备份数据库」手动补一次");
            L.push("  3. 看日志：D:\\zotero-tools\\logs\\backup.log");
            L.push("  4. 重设凭证：python D:\\zotero-tools\\backup_db.py --set-password");
        } else {
            L.push("");
            L.push("说明：快照内容没变时不会重复上传，这是正常的。");
            L.push("      附件不在这里备份 —— 它们由 Zotero 的 WebDAV 同步单独上传。");
        }

        L.push("");
        L.push("详细日志：D:\\zotero-tools\\logs\\backup.log");
        return L.join("\n");
    }

    function showBackupStatus(silent) {
        var warnHours = parseInt(getPref(WARN_AFTER_HOURS_PREF, 48), 10);
        if (isNaN(warnHours) || warnHours < 1) warnHours = 48;
        return readStatus().then(function (st) {
            var text = renderBackupStatus(st, backupProblems(st, warnHours));
            log("backup status report:\n" + text);
            if (!silent) {
                try {
                    Zotero.alert(null, "Dual Sync — 数据库备份状态", text);
                } catch (e) {}
            }
            return text;
        });
    }

    function syncOnce(options) {        options = options || {};
        if (_running) return Promise.resolve(_lastSummary);
        _running = true;

        var linked = 0;
        var errors = 0;
        var doneCount = 0;
        var todoCount = 0;
        var doneIds = {};
        var todoIds = {};
        var win = null;

        try {
            if (!options.silent && typeof Zotero.ProgressWindow === "function") {
                win = new Zotero.ProgressWindow();
                win.changeHeadline("Dual Sync");
                win.show();
            }
        } catch (e) {}

        function finish() {
            _running = false;
            _lastSummary =
                "已挂 dual " + linked + " · 已译 " + doneCount + " · 待译 " + todoCount + (errors ? " · 错误 " + errors : "");
            log(_lastSummary);
            if (win) {
                try {
                    win.addDescription(_lastSummary);
                    win.addDescription("标签：绿=已译 / 红=待译（打在条目上）");
                    win.startCloseTimer(4000);
                } catch (e) {}
            }
            return _lastSummary;
        }

        return ensureTagColors()
            .then(function () {
                return removeUnwantedSearches();
            })
            .then(function () {
                var dataDir = Zotero.DataDirectory.dir;
                return listChildren(joinPath(dataDir, "storage"));
            })
            .then(function (folders) {
                var idx = 0;
                function nextFolder() {
                    if (idx >= folders.length) return Promise.resolve();
                    var folderPath = folders[idx++];
                    return isDirectory(folderPath)
                        .then(function (dir) {
                            if (!dir) return null;
                            var key = basename(folderPath);
                            if (!key || key.length < 5) return null;
                            // Cheap pre-filter: an unchanged folder mtime means no
                            // file was added or removed, so skip listing its files and
                            // replay the cached decision instead (keeps the summary
                            // counts right without touching the filesystem further).
                            return statMtime(folderPath).then(function (mtime) {
                                var cached = _folderState[key];
                                if (mtime && cached && cached.mtime === mtime) {
                                    if (cached.state === "done") doneIds[cached.parent] = 1;
                                    else if (cached.state === "todo") todoIds[cached.parent] = 1;
                                    return null;
                                }
                                return listChildren(folderPath).then(function (children) {
                                var duals = [];
                                var origs = [];
                                var j = 0;
                                function nextChild() {
                                    if (j >= children.length) {
                                        return findItemByKey(key).then(function (item) {
                                            if (!item) return null;
                                            return parentRegularOf(item).then(function (parent) {
                                                if (!parent) return null;
                                                // never count or tag a trashed item
                                                if (parent.deleted) return null;
                                                return parentHasDualAtt(parent).then(function (hasDualAtt) {
                                                    if (duals.length && !hasDualAtt) {
                                                        duals.sort(function (a, b) {
                                                            return basename(a).length - basename(b).length;
                                                        });
                                                        var dualPath = duals[0];
                                                        var base = origs.length
                                                            ? basename(origs[0]).replace(/\.pdf$/i, "")
                                                            : basename(dualPath).replace(
                                                                  /(\.no_watermark\.zh-cn)?\.dual\.pdf$/i,
                                                                  "",
                                                              );
                                                        var label = "双语对照 · " + String(base).slice(0, 80);
                                                        return linkDual(dualPath, parent, label)
                                                            .then(function () {
                                                                linked++;
                                                                doneIds[parent.id] = 1;
                                                                _folderState[key] = { mtime: mtime, state: "done", parent: parent.id };
                                                                if (win) {
                                                                    try {
                                                                        win.addDescription(label);
                                                                    } catch (e) {}
                                                                }
                                                            })
                                                            .catch(function () {
                                                                // deliberately NOT cached: a failed link
                                                                // should be retried on the next pass
                                                                errors++;
                                                                doneIds[parent.id] = 1;
                                                            });
                                                    }
                                                    if (duals.length || hasDualAtt) {
                                                        doneIds[parent.id] = 1;
                                                        _folderState[key] = { mtime: mtime, state: "done", parent: parent.id };
                                                        return null;
                                                    }
                                                    var title = itemTitle(parent);
                                                    var enName = origs.length > 0 && looksEnglish(basename(origs[0]));
                                                    if (looksEnglish(title) || enName) {
                                                        todoIds[parent.id] = 1;
                                                        _folderState[key] = { mtime: mtime, state: "todo", parent: parent.id };
                                                    } else {
                                                        _folderState[key] = { mtime: mtime, state: "none", parent: parent.id };
                                                        return setStatusTag(parent, "none");
                                                    }
                                                });
                                            });
                                        });
                                    }
                                    var p = children[j++];
                                    return isDirectory(p)
                                        .then(function (d) {
                                            if (d) return null;
                                            var bn = basename(p);
                                            if (isDualFilename(bn)) duals.push(p);
                                            else if (isOriginalPdf(bn)) origs.push(p);
                                        })
                                        .catch(function () {})
                                        .then(nextChild);
                                }
                                return nextChild();
                                });
                            });
                        })
                        .catch(function () {})
                        .then(nextFolder);
                }
                return nextFolder();
            })
            .then(function () {
                var doneKeys = Object.keys(doneIds);
                var todoKeys = Object.keys(todoIds);
                var di = 0;
                var ti = 0;
                function tagDone() {
                    if (di >= doneKeys.length) return tagTodo();
                    var id = parseInt(doneKeys[di++], 10);
                    return Zotero.Items.getAsync(id)
                        .then(function (item) {
                            doneCount++;
                            return setStatusTag(item, "done");
                        })
                        .catch(function () {})
                        .then(tagDone);
                }
                function tagTodo() {
                    if (ti >= todoKeys.length) return null;
                    var id = parseInt(todoKeys[ti++], 10);
                    if (doneIds[id]) return tagTodo();
                    return Zotero.Items.getAsync(id)
                        .then(function (item) {
                            return setStatusTag(item, "todo").then(function () {
                                todoCount++;
                            });
                        })
                        .catch(function () {})
                        .then(tagTodo);
                }
                return tagDone();
            })
            .then(function () {
                // Attaching/tagging is done. Whatever is still in todoIds is an
                // English PDF with no dual -> hand it to the translator.
                // Deliberately NOT awaited: a translation takes minutes and must
                // not hold up the sync summary or the progress window.
                try {
                    if (Object.keys(todoIds).length) {
                        autoTranslatePending(todoIds, false);
                    }
                } catch (e) {
                    log("auto-translate dispatch error: " + e);
                }
                return null;
            })
            .then(finish)
            .catch(function (e) {
                log("sync error: " + e);
                errors++;
                return finish();
            });
    }

    function showStats() {
        var done = 0;
        var todo = 0;
        return Promise.resolve()
            .then(function () {
                var libs = Zotero.Libraries.getAll();
                var jobs = [];
                libs.forEach(function (lib) {
                    jobs.push(
                        Promise.resolve()
                            .then(function () {
                                if (lib.libraryType && lib.libraryType !== "user") return;
                                return Zotero.Items.getAllIDs(lib.libraryID).then(function (ids) {
                                    var i = 0;
                                    function next() {
                                        if (i >= ids.length) return Promise.resolve();
                                        var id = ids[i++];
                                        return Zotero.Items.getAsync(id)
                                            .then(function (item) {
                                                if (!item || item.deleted || !isRegular(item)) return null;
                                                if (item.hasTag(TAG_DONE)) done++;
                                                if (item.hasTag(TAG_TODO)) todo++;
                                            })
                                            .catch(function () {})
                                            .then(next);
                                    }
                                    return next();
                                });
                            })
                            .catch(function () {}),
                    );
                });
                return Promise.all(jobs);
            })
            .then(function () {
                try {
                    var win = new Zotero.ProgressWindow();
                    win.changeHeadline("Dual Sync 统计");
                    win.addDescription("🟢 " + TAG_DONE + "：" + done);
                    win.addDescription("🔴 " + TAG_TODO + "：" + todo);
                    win.show();
                    win.startCloseTimer(4000);
                } catch (e) {}
                return { done: done, todo: todo };
            })
            .catch(function () {
                return { done: 0, todo: 0 };
            });
    }

    // -----------------------------------------------------------------------
    // Right-click actions: exclude a COLLECTION, or tag a single ITEM.
    //
    // Both write the SAME shared state the batch script reads, so there is exactly
    // one source of truth:
    //   * collection -> exclude_collections.json (the file LitFlow's settings page
    //     and batch_translate_live.py already use)
    //   * item       -> a tag, whose name comes from config.json -> "exclude_tag"
    //
    // The file write is atomic (temp file + move).  A torn write would be read by
    // the batch script and by LitFlow as "no exclusions", silently translating
    // everything the user had excluded.
    // -----------------------------------------------------------------------

    var MENU_COLL_EXCLUDE = "dualsync-menuitem-coll-exclude";
    var MENU_ITEM_TAG = "dualsync-menuitem-item-tag";

    function excludeFilePath() {
        try {
            var cfgFile = toFile(String(getPref(EXCLUDE_PREF, DEFAULT_CONFIG) || DEFAULT_CONFIG));
            if (!cfgFile || !cfgFile.exists()) return null;
            var cfg = JSON.parse(Zotero.File.getContents(cfgFile));
            return cfg.exclude_collections ? toFile(String(cfg.exclude_collections)) : null;
        } catch (e) {
            log("exclude: 找不到排除配置路径 (" + e + ")");
            return null;
        }
    }

    function excludeTagName() {
        try {
            var cfgFile = toFile(String(getPref(EXCLUDE_PREF, DEFAULT_CONFIG) || DEFAULT_CONFIG));
            if (!cfgFile || !cfgFile.exists()) return "";
            var cfg = JSON.parse(Zotero.File.getContents(cfgFile));
            return String(cfg.exclude_tag || "").trim();
        } catch (e) {
            return "";
        }
    }

    /** Read {_readme, version, exclude:[{key,name}]}.  Never throws. */
    function readExcludeFile() {
        try {
            var f = excludeFilePath();
            if (!f || !f.exists()) return null;
            var data = JSON.parse(Zotero.File.getContents(f));
            return {
                path: f.path,
                _readme: data._readme,
                version: data.version,
                exclude: (data.exclude || []).filter(function (e) { return e && e.key; }),
            };
        } catch (e) {
            log("exclude: 读取失败 (" + e + ")");
            return null;
        }
    }

    /** True when the exclusion file on disk holds exactly the keys in `state`.
     *
     *  The caller's message ("已从翻译中排除") is a claim about what is on disk, so
     *  it is verified rather than assumed: on Windows a concurrent reader (the
     *  batch script, LitFlow) can make the rename fail without any exception ever
     *  reaching us.
     */
    function excludeFileMatches(state) {
        var back = readExcludeFile();
        if (!back) return false;
        function keys(list) {
            return (list || []).map(function (e) { return e.key; }).sort().join(",");
        }
        var want = keys(state.exclude);
        var got = keys(back.exclude);
        if (want !== got) {
            log("exclude: 写回校验不一致（期望 [" + want + "]，实际 [" + got + "]）");
        }
        return want === got;
    }

    /**
     * Atomic replace.  Returns a **promise** for true on success.
     *
     * IOUtils.writeUTF8 + IOUtils.move is the modern path; older Zotero falls back to a
     * plain write.  Either way the payload is re-parsed before it is written, so a
     * bug here cannot leave the file unreadable for the batch script.
     *
     * Both IOUtils calls MUST be awaited.  They return promises, and issuing move()
     * while the write is still in flight is exactly how this goes wrong: the rename
     * either cannot find the temp file, or fails (Windows: the destination is being
     * read) and the rejection never reaches the old try/catch — while the caller
     * happily tells the user the exclusion is in effect.  That is the silent
     * failure this function exists to avoid, so failures now fall through to the
     * plain write and the result is read back before it is reported.
     */
    function writeExcludeFileAtomic(state) {
        var payload = {
            _readme: state._readme,
            version: state.version,
            exclude: state.exclude,
        };
        var text = JSON.stringify(payload, null, 2) + "\n";
        try {
            JSON.parse(text);
        } catch (e) {
            log("exclude: 生成的 JSON 不合法，已放弃写入");
            return Promise.resolve(false);
        }
        var target = state.path;
        var tmp = target + "." + Date.now() + ".tmp";

        /** Plain (non-atomic) last resort, still verified. */
        function plainWrite() {
            try {
                Zotero.File.putContents(toFile(target), text);
            } catch (e) {
                log("exclude: 写入失败，原文件未改动 (" + e + ")");
                return false;
            }
            return excludeFileMatches(state);
        }

        if (typeof IOUtils !== "undefined" && IOUtils.writeUTF8) {
            return IOUtils.writeUTF8(tmp, text)
                .then(function () {
                    return IOUtils.move(tmp, target);
                })
                .then(function () {
                    if (excludeFileMatches(state)) return true;
                    log("exclude: 原子写已返回但文件内容不符，改用普通写");
                    return plainWrite();
                })
                .catch(function (e) {
                    log("exclude: IOUtils 原子写失败，改用普通写 (" + e + ")");
                    return IOUtils.remove(tmp, { ignoreAbsent: true })
                        .catch(function () {})
                        .then(function () {
                            return plainWrite();
                        });
                });
        }
        return Promise.resolve(plainWrite());
    }

    /** Every collection in the same library that shares this collection's NAME. */
    function sameNameCollections(coll) {
        var out = [coll];
        try {
            var all = Zotero.Collections.getByLibrary(coll.libraryID, true) || [];
            all.forEach(function (c) {
                if (c.id !== coll.id && c.name === coll.name) out.push(c);
            });
        } catch (e) {
            log("exclude: 取同名分类失败，只处理选中的那个 (" + e + ")");
        }
        return out;
    }

    function toggleCollectionExclusion() {
        var state = readExcludeFile();
        if (!state) {
            Zotero.alert(null, "Dual Sync", "读不到排除配置，无法修改。\n" +
                "请检查 config.json 里的 exclude_collections。");
            return;
        }
        var coll = null;
        try {
            coll = Zotero.getMainWindow().ZoteroPane.getSelectedCollection();
        } catch (e) {}
        if (!coll) {
            Zotero.alert(null, "Dual Sync", "请先选中一个分类。");
            return;
        }
        // Both parallel trees: on this machine every name maps to two keys and the
        // two trees have zero item overlap, so excluding only the clicked one would
        // leave the other tree's papers translated.
        var family = sameNameCollections(coll);
        var keys = family.map(function (c) { return c.key; });
        var present = keys.filter(function (k) {
            return state.exclude.some(function (e) { return e.key === k; });
        });
        var removing = present.length === keys.length;

        if (removing) {
            state.exclude = state.exclude.filter(function (e) {
                return keys.indexOf(e.key) === -1;
            });
        } else {
            family.forEach(function (c) {
                if (!state.exclude.some(function (e) { return e.key === c.key; })) {
                    state.exclude.push({ key: c.key, name: c.name || c.key });
                }
            });
        }
        var extra = keys.length > 1
            ? "\n（该名称对应 " + keys.length + " 个分类，已一起处理）"
            : "";
        // writeExcludeFileAtomic now returns a promise (the IOUtils calls have to be
        // awaited), so the alert reports the VERIFIED result instead of a hopeful one.
        // Promise.resolve() also catches a synchronous throw, which the old
        // try/catch-around-a-boolean shape could not.
        Promise.resolve()
            .then(function () {
                return writeExcludeFileAtomic(state);
            })
            .catch(function (e) {
                log("exclude: 写入异常 (" + e + ")");
                return false;
            })
            .then(function (ok) {
                Zotero.alert(null, "Dual Sync",
                    ok
                        ? (removing ? "已恢复翻译：" : "已从翻译中排除：") + coll.name + extra
                        : "写入失败，配置未改动。" + extra);
                if (ok) _excludeCache = null;   // the drain loop must see the new list at once
            });
    }

    function toggleItemTag() {
        var tag = excludeTagName();
        if (!tag) {
            Zotero.alert(null, "Dual Sync",
                "config.json 里没有配置 exclude_tag，无法打标签。");
            return;
        }
        var items = [];
        try {
            items = Zotero.getMainWindow().ZoteroPane.getSelectedItems() || [];
        } catch (e) {}
        items = items.filter(function (i) { return i && i.isRegularItem; });
        if (!items.length) {
            Zotero.alert(null, "Dual Sync", "请先选中至少一条文献（附件不算）。");
            return;
        }
        // Toggling: if every selected item already has it, remove; otherwise add.
        // Round-trippable on purpose — a wrong click must be undoable.
        var allHave = items.every(function (i) { return i.hasTag(tag); });
        items.forEach(function (i) {
            try {
                if (allHave) i.removeTag(tag);
                else i.addTag(tag);
                i.saveTx();
            } catch (e) {
                log("tag: " + i.key + " 失败 (" + e + ")");
            }
        });
        Zotero.alert(null, "Dual Sync",
            (allHave ? "已恢复翻译（去掉标签 " : "已标记为不翻译（标签 ") + tag + "）：" +
            items.length + " 条");
    }

    function ensureContextMenus(window) {
        try {
            if (!window || !window.document) return;
            var doc = window.document;

            var collMenu = doc.getElementById("zotero-collectionmenu");
            if (collMenu && !doc.getElementById(MENU_COLL_EXCLUDE)) {
                var ci = doc.createXULElement
                    ? doc.createXULElement("menuitem")
                    : doc.createElement("menuitem");
                ci.id = MENU_COLL_EXCLUDE;
                ci.setAttribute("label", "Dual Sync：从翻译中排除 / 恢复此分类");
                ci.addEventListener("command", function () { toggleCollectionExclusion(); });
                collMenu.appendChild(ci);
            }

            var itemMenu = doc.getElementById("zotero-itemmenu");
            if (itemMenu && !doc.getElementById(MENU_ITEM_TAG)) {
                var ii = doc.createXULElement
                    ? doc.createXULElement("menuitem")
                    : doc.createElement("menuitem");
                ii.id = MENU_ITEM_TAG;
                ii.setAttribute("label", "Dual Sync：这篇不翻译 / 恢复翻译");
                ii.addEventListener("command", function () { toggleItemTag(); });
                itemMenu.appendChild(ii);
            }
        } catch (e) {
            log("context menu: " + e);
        }
    }

    function ensureMenu(window) {
        try {
            if (!window || !window.document) return;
            var doc = window.document;
            if (doc.getElementById(MENU_SYNC)) return;
            var popup = doc.getElementById("menu_ToolsPopup");
            if (!popup) return;

            var sep = doc.createXULElement
                ? doc.createXULElement("menuseparator")
                : doc.createElement("menuseparator");
            sep.id = MENU_SYNC + "-sep";
            popup.appendChild(sep);

            function add(id, label, fn) {
                var mi = doc.createXULElement
                    ? doc.createXULElement("menuitem")
                    : doc.createElement("menuitem");
                mi.id = id;
                mi.setAttribute("label", label);
                mi.addEventListener("command", fn);
                popup.appendChild(mi);
            }

            add(MENU_SYNC, "Dual Sync：同步 dual 并着色", function () {
                syncOnce({ silent: false });
            });
            add(MENU_STATS, "Dual Sync：统计已译/待译", function () {
                showStats();
            });
            add(MENU_TRANSLATE, "Dual Sync：翻译选中条目", function () {
                translateSelected();
            });
            add(MENU_BACKUP, "Dual Sync：立即备份数据库", function () {
                backupNow("menu");
            });
            add(MENU_BACKUP_STATUS, "Dual Sync：数据库备份状态", function () {
                showBackupStatus();
            });
        } catch (e) {
            log("menu: " + e);
        }
    }

    function removeMenu(window) {
        try {
            var doc = window && window.document;
            if (!doc) return;
            [MENU_SYNC, MENU_SYNC + "-sep", MENU_STATS, MENU_STATS + "-sep",
             MENU_TRANSLATE, MENU_BACKUP, MENU_BACKUP_STATUS].forEach(function (id) {
                var el = doc.getElementById(id);
                if (el && el.parentNode) el.parentNode.removeChild(el);
            });
        } catch (e) {}
    }

    function onMainWindowLoad(window) {
        try {
            if (!window) return;
            if (_hookedWindows) {
                if (_hookedWindows.has(window)) return;
                _hookedWindows.add(window);
            }
            window.setTimeout(function () {
                ensureMenu(window);
        ensureContextMenus(window);
            }, 1000);
            window.setTimeout(function () {
                ensureMenu(window);
        ensureContextMenus(window);
            }, 4000);
        } catch (e) {}
    }

    /** Remove the two context-menu items from one window.
     *
     * Called on window unload AND from shutdown(): a plugin disable/update does
     * NOT unload the window, so removing them only here left the two menuitems
     * alive with listeners bound to an already-shut-down scope — and because
     * ensureContextMenus() skips anything whose id already exists, re-enabling
     * the plugin would keep the stale closure rather than install a fresh one.
     */
    function removeContextMenuItems(window) {
        try {
            var doc = window && window.document;
            if (!doc) return;
            [MENU_COLL_EXCLUDE, MENU_ITEM_TAG].forEach(function (id) {
                var el = doc.getElementById(id);
                if (el && el.parentNode) el.parentNode.removeChild(el);
            });
        } catch (e) {}
    }

    function onMainWindowUnload(window) {
        removeMenu(window);
        removeContextMenuItems(window);
    }

    function autoSyncEnabled() {
        return getPref(AUTO_SYNC_PREF, true) !== false;
    }

    // ------------------------------------------------------------------ events
    //
    // IMPORTANT: this only fires for changes made *inside* Zotero. A dual PDF
    // written into storage/ by the external pdf2zh pipeline produces no Zotero
    // event whatsoever, so the filesystem poll CANNOT simply be removed - it is
    // what notices those. The notifier just makes Zotero-side changes (imports,
    // manually added attachments) show up at once instead of at the next poll,
    // and the poll now skips folders whose mtime is unchanged.
    function scheduleSync(reason) {
        if (!autoSyncEnabled()) return;
        if (_debounce) {
            try {
                clearTimeout(_debounce);
            } catch (e) {}
        }
        _debounce = setTimeout(function () {
            _debounce = null;
            syncOnce({ silent: true }).then(
                function (s) {
                    log("sync after " + reason + ": " + s);
                },
                function (e) {
                    log("sync after " + reason + " failed: " + e);
                },
            );
        }, NOTIFY_DEBOUNCE_MS);
    }

    function registerNotifier() {
        if (_notifierRef) return;
        try {
            _notifierRef = {
                notify: function (event, type) {
                    try {
                        if (type !== "item") return;
                        if (event !== "add" && event !== "modify") return;
                        scheduleSync("notifier:" + event);
                    } catch (e) {}
                },
            };
            Zotero.Notifier.registerObserver(_notifierRef, ["item"], NOTIFIER_ID, 100);
            log("notifier registered");
        } catch (e) {
            _notifierRef = null;
            log("notifier registration failed: " + e);
        }
    }

    function unregisterNotifier() {
        try {
            if (_notifierRef) Zotero.Notifier.unregisterObserver(_notifierRef);
        } catch (e) {}
        _notifierRef = null;
        if (_debounce) {
            try {
                clearTimeout(_debounce);
            } catch (e) {}
            _debounce = null;
        }
    }

    function pollSeconds() {
        // NOTE: the default here must be a value that means "unset". Passing 0
        // made an unset pref look like "poll disabled", which silently turned
        // off the only mechanism that notices duals written into storage/ by
        // the external pipeline.
        var raw = getPref(POLL_SECONDS_PREF, null);
        if (raw === null || raw === undefined || raw === "") {
            return Math.round(SYNC_INTERVAL_MS / 1000);
        }
        var v = parseInt(raw, 10);
        if (isNaN(v) || v < 0) return Math.round(SYNC_INTERVAL_MS / 1000);
        return v;
    }

    function init(data) {
        log("init v" + (data && data.version ? data.version : "?"));
        try {
            var windows = Zotero.getMainWindows();
            for (var i = 0; i < windows.length; i++) onMainWindowLoad(windows[i]);
        } catch (e) {
            log("init windows: " + e);
        }

        // keep Zotero's backup retention/interval in sync with our prefs
        applyBackupPrefs();

        // Verify the snapshot-discovery glue at startup WITHOUT creating a
        // backup (no churn from frequent restarts). The manual menu action uses
        // the same code path.
        findNewestBak().then(function (bak) {
            if (bak) {
                log(
                    "backup: newest snapshot " + basename(bak.path) + " " +
                        (bak.size / 1e6).toFixed(2) + " MB, " +
                        ((Date.now() - bak.mtime) / 3.6e6).toFixed(1) + " h old",
                );
            } else {
                log("backup: no zotero.sqlite*.bak found in the data directory");
            }
        });

        if (_syncTimeout) {
            try {
                clearTimeout(_syncTimeout);
            } catch (e) {}
            _syncTimeout = null;
        }
        if (_timer) {
            try {
                clearInterval(_timer);
            } catch (e) {}
            _timer = null;
        }

        if (!autoSyncEnabled()) {
            log("auto-sync disabled by pref " + AUTO_SYNC_PREF);
            unregisterNotifier();
            return;
        }

        // Delayed auto-sync — fully silent, never throws.
        // Kept in _syncTimeout so shutdown() can cancel it (the old code let it
        // fire after the add-on had already been unloaded).
        _syncTimeout = setTimeout(function () {
            _syncTimeout = null;
            syncOnce({ silent: true }).then(
                function (s) {
                    log("auto sync: " + s);
                },
                function (e) {
                    log("auto sync fail: " + e);
                },
            );
        }, 15000);

        // Warn early if the backup chain looks broken. Delayed so it never
        // competes with startup, and shown at most once per session.
        _healthTimeout = setTimeout(function () {
            _healthTimeout = null;
            checkBackupHealth();
        }, 30000);

        // One-shot self tests for the backup paths (same code the menu runs,
        // minus the dialogs). Off unless explicitly enabled.
        if (getPref("dualsync.testBackupOnStartup", false) === true) {
            setTimeout(function () {
                log("backup: TEST hook firing backupNow('menu', silent)");
                backupNow("menu", true).then(function (s) {
                    log("backup: TEST result " + JSON.stringify(s));
                });
            }, 20000);
        }
        if (getPref("dualsync.testStatusOnStartup", false) === true) {
            setTimeout(function () {
                log("backup: TEST hook rendering the status dialog (silent)");
                showBackupStatus(true);
            }, 25000);
        }

        var secs = pollSeconds();
        if (secs > 0) {
            _timer = setInterval(function () {
                syncOnce({ silent: true }).then(
                    function () {},
                    function () {},
                );
            }, secs * 1000);
            log("filesystem poll every " + secs + "s (unchanged folders skipped)");
        } else {
            log("filesystem poll disabled (pollSeconds=0)");
        }

        // Zotero-side changes trigger a debounced sync immediately. This does
        // NOT see files written into storage/ by the external pipeline.
        registerNotifier();

        Zotero.DualSync = {
            init: init,
            shutdown: shutdown,
            syncNow: syncOnce,
            showStats: showStats,
            translateSelected: translateSelected,
            backupNow: backupNow,
            showBackupStatus: showBackupStatus,
            onMainWindowLoad: onMainWindowLoad,
            onMainWindowUnload: onMainWindowUnload,
            getLastSummary: function () {
                return _lastSummary;
            },
            TAG_DONE: TAG_DONE,
            TAG_TODO: TAG_TODO,
        };
    }

    function shutdown() {
        unregisterNotifier();
        try {
            if (_healthTimeout) clearTimeout(_healthTimeout);
        } catch (e) {}
        _healthTimeout = null;
        try {
            if (_timer) clearInterval(_timer);
        } catch (e) {}
        _timer = null;
        try {
            if (_syncTimeout) clearTimeout(_syncTimeout);
        } catch (e) {}
        _syncTimeout = null;
        try {
            var windows = Zotero.getMainWindows();
            for (var i = 0; i < windows.length; i++) {
                removeMenu(windows[i]);
                removeContextMenuItems(windows[i]);
            }
        } catch (e) {}
        try {
            delete Zotero.DualSync;
        } catch (e) {}
    }

    Zotero.DualSync = {
        init: init,
        shutdown: shutdown,
        syncNow: syncOnce,
        showStats: showStats,
        onMainWindowLoad: onMainWindowLoad,
        onMainWindowUnload: onMainWindowUnload,
        getLastSummary: function () {
            return _lastSummary;
        },
        TAG_DONE: TAG_DONE,
        TAG_TODO: TAG_TODO,
    };
})();
