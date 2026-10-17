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
	"encoding/json"
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
	installed, active := zoteroPluginState()
	// 网关地址：插件要把 prefs.js 里的 litflowUrl 指到这里。
	// 用请求的 Host 而不是写死 127.0.0.1，因为面板可能从另一台机器打开。
	host := r.Host
	if host == "" {
		host = "127.0.0.1:7863"
	}
	writeJSONQ(w, http.StatusOK, map[string]any{
		"zotero_running":   running,
		"plugin_present":   hasPlugin,
		"plugin_installed": installed,
		"plugin_active":    active,
		"gateway_url":      "http://" + host,
		"expected_url":     "http://127.0.0.1:7863",
		"hint":             hintForZotero(running, hasPlugin, installed, active),
		"plugin_file":      filepath.Base(path),
	})
}

// pluginID 与 manifest 的 applications.zotero.id 必须一致。
const pluginID = "dualsync@zotero.local"

// zoteroPluginState 读 Zotero 自己的 extensions.json，判断插件是否已安装。
//
// 为什么值得做：面板此前只能说「Zotero 在运行」和「我手上有安装包」—— 而这两件
// 都成立、插件却没装的情况，正是我们花了四轮才排掉的那个状态。用户看到「插件包
// 已就绪」会以为装好了，实际 Zotero 里什么都没有。
//
// 读扩展数据库是只读的；Zotero 运行中它也已经持有上一次的落盘内容，所以不要求
// Zotero 关闭。读不到一律返回 false —— 那表示「不知道」，而调用方要把它与
// 「确实没装」区分开（见 hint 的分支）。
func zoteroPluginState() (installed, active bool) {
	for _, prof := range zoteroProfileDirs() {
		raw, err := os.ReadFile(filepath.Join(prof, "extensions.json"))
		if err != nil {
			continue
		}
		var doc struct {
			Addons []struct {
				ID     string `json:"id"`
				Active bool   `json:"active"`
				// Zotero 把「被禁用」拆成两个来源：用户关的，以及应用判定不兼容
				// 而关的。后者才说明这个包有问题，所以两个都读。
				AppDisabled bool `json:"appDisabled"`
			} `json:"addons"`
		}
		if json.Unmarshal(raw, &doc) != nil {
			continue
		}
		for _, a := range doc.Addons {
			if a.ID == pluginID {
				return true, a.Active && !a.AppDisabled
			}
		}
	}
	return false, false
}

// zoteroProfileDirs 找 Zotero profile 目录。
//
// 先读 profiles.ini（权威来源），读不到再扫 Profiles/ 兜底。
func zoteroProfileDirs() []string {
	appdata := os.Getenv("APPDATA")
	if appdata == "" {
		return nil
	}
	base := filepath.Join(appdata, "Zotero", "Zotero")
	var out []string
	if raw, err := os.ReadFile(filepath.Join(base, "profiles.ini")); err == nil {
		for _, line := range strings.Split(string(raw), "\n") {
			line = strings.TrimSpace(line)
			if !strings.HasPrefix(line, "Path=") {
				continue
			}
			rel := strings.TrimSpace(strings.TrimPrefix(line, "Path="))
			// ini 里写的是正斜杠；FromSlash 让 Windows 上也能解析。
			p := filepath.Join(base, filepath.FromSlash(rel))
			if st, err := os.Stat(p); err == nil && st.IsDir() {
				out = append(out, p)
			}
		}
	}
	if len(out) == 0 {
		if entries, err := os.ReadDir(filepath.Join(base, "Profiles")); err == nil {
			for _, e := range entries {
				if !e.IsDir() {
					continue
				}
				p := filepath.Join(base, "Profiles", e.Name())
				if _, err := os.Stat(filepath.Join(p, "prefs.js")); err == nil {
					out = append(out, p)
				}
			}
		}
	}
	return out
}

// hintForZotero 一句人话，直接告诉用户下一步做什么。
//
// 「傻瓜式」的实质就在这里：不是把状态摆出来让人自己推，而是把**下一步**说出来。
// 每条组合对应一个不同的动作，尤其是「装了但没启用」—— 那一条此前完全看不到，
// 而它的表现（没有菜单）和「没装」一模一样。
func hintForZotero(running, hasPlugin, installed, active bool) string {
	switch {
	case installed && active:
		return "插件已安装并启用。在文献列表里选中条目，然后 工具 → 「Dual Sync：翻译选中条目」。"
	case installed && !active:
		return "插件已安装，但在 Zotero 里未启用（或已被停用）。到 Zotero → 工具 → 插件 里启用 Zotero Dual Sync，然后重启 Zotero。"
	case !hasPlugin && !running:
		return "没找到插件包（" + pluginFileName + "）。请把它放在网关 exe 同目录，然后刷新本页。"
	case !hasPlugin && running:
		return "Zotero 在运行，但本机没找到插件包（" + pluginFileName + "）。请把它放到网关 exe 同目录。"
	case running:
		return "插件包已就绪、Zotero 在运行，但 Zotero 里还没有这个插件。点「下载插件」，在 工具 → 插件 → 齿轮 → Install Add-on From File 里选它，然后重启 Zotero。"
	default:
		return "插件包已就绪。先启动 Zotero，再点「下载插件」安装，装完重启 Zotero。"
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
	// **必须禁缓存**：修好 zip 结构之后，用户重下时如果拿到浏览器缓存里的旧包，
	// 会再次看到一模一样的「可能无法与该版本的 Zotero 兼容」，而我们两个都会
	// 以为修复无效。这个坑不值得再踩一次。
	w.Header().Set("Cache-Control", "no-store, must-revalidate")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Expires", "0")
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
					"version":     "1.1.1",
					"update_link": "http://" + r.Host + "/panel/api/zotero/plugin",
				}},
			},
		},
	})
}
