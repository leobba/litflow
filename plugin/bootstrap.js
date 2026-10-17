/* eslint-disable no-undef */
// Minimal bootstrap — everything wrapped so a failure cannot brick Zotero UI
var chromeHandle;

function install(data, reason) {}

async function startup(data, reason) {
    try {
        await Zotero.initializationPromise;
    } catch (e) {
        // ignore
    }

    try {
        var rootURI = data.rootURI;
        if (!rootURI && data.resourceURI) {
            rootURI = data.resourceURI.spec;
        }
        if (!rootURI) {
            throw new Error("DualSync: no rootURI");
        }
        // normalize: ensure single slash join
        if (rootURI.charAt(rootURI.length - 1) === "/") {
            rootURI = rootURI.slice(0, -1);
        }
        var scriptPath = rootURI + "/content/dualsync.js";

        var ctx = {
            rootURI: rootURI + "/",
        };
        ctx._globalThis = ctx;
        Services.scriptloader.loadSubScript(scriptPath, ctx);

        if (!Zotero.DualSync || typeof Zotero.DualSync.init !== "function") {
            throw new Error("DualSync: init not found after loadSubScript");
        }
        Zotero.DualSync.init({ id: data.id, version: data.version, rootURI: rootURI + "/" });
        Zotero.debug("[DualSync] started ok " + data.version);
    } catch (e) {
        // Do NOT rethrow — keep Zotero usable
        try {
            Zotero.debug("[DualSync] startup error (ignored): " + e);
            Zotero.logError(e);
        } catch (e2) {}
    }
}

function onMainWindowLoad({ window }, reason) {
    try {
        if (Zotero.DualSync && Zotero.DualSync.onMainWindowLoad) {
            Zotero.DualSync.onMainWindowLoad(window);
        }
    } catch (e) {
        try {
            Zotero.debug("[DualSync] onMainWindowLoad error: " + e);
        } catch (e2) {}
    }
}

function onMainWindowUnload({ window }, reason) {
    try {
        if (Zotero.DualSync && Zotero.DualSync.onMainWindowUnload) {
            Zotero.DualSync.onMainWindowUnload(window);
        }
    } catch (e) {}
}

function shutdown(data, reason) {
    try {
        if (reason === APP_SHUTDOWN) {
            return;
        }
        if (typeof Zotero === "undefined") {
            Zotero = Components.classes["@zotero.org/Zotero;1"].getService(
                Components.interfaces.nsISupports,
            ).wrappedJSObject;
        }
        if (Zotero.DualSync && Zotero.DualSync.shutdown) {
            Zotero.DualSync.shutdown();
        }
    } catch (e) {}
    try {
        if (chromeHandle) {
            chromeHandle.destruct();
            chromeHandle = null;
        }
    } catch (e) {}
}

function uninstall(data, reason) {}
