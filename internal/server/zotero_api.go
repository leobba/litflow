package server

// Zotero 集成的三个端点，目的是把「装插件」这件事从一串手工步骤变成两次点击。
//
//	GET  /panel/api/zotero/status    Zotero 在不在 + 插件包在不在 + 网关地址
//	GET  /panel/api/zotero/plugin    下载插件 xpi
//	POST /panel/api/zotero/enable    尝试打开 Zotero 的本地 API（可选能力）
//
// ## 为什么「状态」要问 Zotero 而不是问插件
//
// 插件装在 Zotero 里，网关看不到它。能确认的只有两件事：Zotero 进程在不在
// （`/connector/ping`），以及我们手上有没有一个可装的 xpi。把这两件事如实显示，
// 比编一个「已连接」诚实——用户装没装插件我们并不知道，而一个假装知道的界面
// 会让真正的失败无从下手。
//
// ## 为什么下载走网关而不是给个文件路径
//
// 用户拿到的通常只有一个 exe 和一个面板地址。让他在文件系统里找 xpi 是把
// 「傻瓜式」交还给资源管理器。面板里点一下就能下载，装完刷新即用。

import (
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// zoteroPingURL Zotero 的连接器探活。
//
// 这个端口只在 Zotero 运行时监听，且**不需要**打开本地 API（本地 API 是
// 另一个默认关闭的开关，只影响 /api/ 下的写回接口）。所以用它判断「Zotero
// 在不在」是安全的：不会因为用户没开某个高级选项而误报未运行。
const zoteroPingURL = "http://127.0.0.1:23119/connector/ping"

// pluginFileName 期望的插件包名，与 manifest 的 id 对应。
const pluginFileName = "dualsync@zotero.local.xpi"

// resolvePluginPath 找到插件包。
//
// 三处按顺序找，因为发行形态不同：
//  1. 配置里写死的路径（开发/自定义部署）
//  2. 与 exe 同目录（发行版把它们放在一起）
//  3. 当前工作目录（从解压目录直接跑）
func (h *Handler) resolvePluginPath() (string, error) {
	if p := strings.TrimSpace(h.cfg.ZoteroPluginPath); p != "" {
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p, nil
		}
		return "", os.ErrNotExist
	}
	var candidates []string
	if exe, err := os.Executable(); err == nil {
		candidates = append(candidates, filepath.Join(filepath.Dir(exe), pluginFileName))
	}
	if wd, err := os.Getwd(); err == nil {
		candidates = append(candidates, filepath.Join(wd, pluginFileName))
	}
	for _, c := range candidates {
		if st, err := os.Stat(c); err == nil && !st.IsDir() {
			return c, nil
		}
	}
	return "", os.ErrNotExist
}

// zoteroRunning 探活 Zotero 的连接器端口。
func zoteroRunning() bool {
	conn, err := net.DialTimeout("tcp", "127.0.0.1:23119", 400*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// zoteroStatus 面板用的一份状态。
func (h *Handler) zoteroStatus(w http.ResponseWriter, r *http.Request) {
	path, err := h.resolvePluginPath()
	hasPlugin := err == nil
	running := zoteroRunning()
	// 网关地址：插件要把 prefs.js 里的 litflowUrl 指到这里。
	// 用请求的 Host 而不是写死 127.0.0.1，因为面板可能从另一台机器打开。
	host := r.Host
	if host == "" {
		host = "127.0.0.1:7863"
	}
	writeJSONQ(w, http.StatusOK, map[string]any{
		"zotero_running": running,
		"plugin_present": hasPlugin,
		"plugin_name":    "",
		"gateway_url":    "http://" + host,
		"expected_url":   "http://127.0.0.1:7863",
		"hint": hintForZotero(running, hasPlugin),
		"plugin_file":    filepath.Base(path),
	})
}

// hintForZotero 一句人话，直接告诉用户下一步做什么。
//
// 「傻瓜式」的实质就在这里：不是把状态摆出来让人自己推，而是把**下一步**
// 说出来。四种组合对应四条不同的动作。
func hintForZotero(running, hasPlugin bool) string {
	switch {
	case !hasPlugin && !running:
		return "没找到插件包（" + pluginFileName + "）。请把它放在网关 exe 同目录，然后刷新本页。"
	case !hasPlugin && running:
		return "Zotero 在运行，但本机没找到插件包（" + pluginFileName + "）。请把它放到网关 exe 同目录。"
	case hasPlugin && !running:
		return "插件包已就绪。先启动 Zotero，再点下面的「下载插件」，在 Zotero 里安装。"
	default:
		return "插件包已就绪、Zotero 在运行。点「下载插件」，在 Zotero 的 工具 → 插件 → 齿轮 → Install Add-on From File 里选它，然后重启 Zotero。"
	}
}

// zoteroPlugin 下发 xpi。
func (h *Handler) zoteroPlugin(w http.ResponseWriter, r *http.Request) {
	path, err := h.resolvePluginPath()
	if err != nil {
		writeErrQ(w, http.StatusNotFound,
			"plugin package not found: put "+pluginFileName+
				" next to the gateway executable, or set zotero.plugin_path in config.json")
		return
	}
	f, err := os.Open(path)
	if err != nil {
		writeErrQ(w, http.StatusInternalServerError, "cannot open plugin: "+err.Error())
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		writeErrQ(w, http.StatusInternalServerError, "cannot stat plugin: "+err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/x-xpinstall")
	// attachment：浏览器直接存盘，而不是尝试渲染一个 xpi。
	w.Header().Set("Content-Disposition",
		`attachment; filename="`+pluginFileName+`"`)
	w.Header().Set("Content-Length", itoa64(st.Size()))
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, f)
}

func itoa64(n int64) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

// zoteroUpdate 下发 Zotero 的更新清单。
//
// 为什么更新源指向本机网关而不是 GitHub：
//
//	manifest 的 applications.zotero.update_url 是**安装时就校验**的字段。
//	实测这台机器上：原值 `https://example.com/zotero-dual-sync/update.json`
//	返回 404，Zotero 10 因此拒绝安装——症状是「装完重启，找不到任何菜单」，
//	而 extensions.json 里连一条记录都没有（被静默移除了）。同一个 profile 里
//	能正常工作的插件，update_url 全部指向真实存在的地址。
//
//	指向本机意味着：只要这个软件在跑，更新源就在。一个 Zotero 插件依赖一个
//	已废弃的域名，是这个项目最不该有的那种失败。
//
// 清单格式（Zotero 扩展更新规范）：
//
//	{"addons": {"<id>": {"updates": [{"version","update_link"}]}}}
func (h *Handler) zoteroUpdate(w http.ResponseWriter, r *http.Request) {
	// 优先读发行目录里的 update.json，这样插件升级时不用重编网关。
	if p, err := h.resolvePluginPath(); err == nil {
		cand := filepath.Join(filepath.Dir(p), "update.json")
		if raw, err := os.ReadFile(cand); err == nil && len(raw) > 0 {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(raw)
			return
		}
	}
	// 兜底：内联一份「已是最新」。update_link 指回插件端点，所以即使
	// 没带 update.json，Zotero 要升级时也能拿到包。
	writeJSONQ(w, http.StatusOK, map[string]any{
		"addons": map[string]any{
			"dualsync@zotero.local": map[string]any{
				"updates": []any{map[string]any{
					"version":     "1.1.0",
					"update_link": "http://" + r.Host + "/panel/api/zotero/plugin",
				}},
			},
		},
	})
}
