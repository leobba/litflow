package server

// 队列 API：面板与 Zotero 插件共用的唯一一组端点。
//
// ## 为什么是「共用一套」而不是各一套
//
// 之前翻译队列有两条接入面：面板走 /panel/api/translate，插件走 /translate。
// 两条面对同一件事（提交、轮询、取消）各写一遍，任何一处改动都要同时想两遍，
// 而「想两遍」正是本项目反复踩到的失真来源。所以这里只留一套根路径端点，
// 面板和插件都调它。
//
//	GET  /health                统一探活（含账号数与队列数）
//	GET  /translate             列表 + 计数 + 轮换链
//	POST /translate             提交（JSON 路径 或 multipart 上传）
//	GET  /translate/{id}        单任务
//	POST /translate/{id}/cancel 取消
//
// ## 鉴权：回环免 key，其余要 key
//
// 插件不带 api_key，而要求用户在 Zotero 的偏好里再填一个 key 是把「傻瓜式」
// 直接破坏掉的一步。但一刀切免鉴权也不行：网关默认监听全部网卡，任何人能连
// 到这台机器就能往队列里塞文件。
//
// 所以按来源分：
//   - 回环（127.0.0.1 / ::1）→ 免 key。这就是「本机插件」这个场景。
//   - 其它来源 → 必须有 key，与 /v1/* 同一套 Bearer。
//
// 这比现状更安全也更好用：现状是全都要 key（插件做不到），而一刀切免 key
// 等于把队列暴露给整个局域网。

import (
	"encoding/json"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/linguo2625469/workbuddy2api-panel/internal/translate"
)

// queueJSON 序列化上限：这些端点收路径/参数，不收文件内容（文件走 multipart）。
const queueJSONLimit = 1 << 20

// isLoopbackRequest 判断请求是否来自本机。
//
// 只看 RemoteAddr：这是 TCP 层的来源，不可被请求头伪造。不看 X-Forwarded-For——
// 那个能伪造，用它做鉴权等于把钥匙交给调用方。
func isLoopbackRequest(r *http.Request) bool {
	host, _, err := net.SplitHostPort(strings.TrimSpace(r.RemoteAddr))
	if err != nil {
		host = strings.TrimSpace(r.RemoteAddr)
	}
	if host == "" {
		return false
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return strings.EqualFold(host, "localhost")
}

// withQueueAuth 队列端点的鉴权：回环放行，其余按 api_key。
func (h *Handler) withQueueAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if isLoopbackRequest(r) {
			next(w, r)
			return
		}
		if !h.apiKeyOK(r) {
			writeOpenAIError(w, http.StatusUnauthorized, "invalid_api_key",
				"this endpoint is open to loopback only; from another host it requires the api_key")
			return
		}
		next(w, r)
	}
}

// withQueueLog 打印每一次队列端点调用。
//
// 为什么值得一行日志：插件的失败形态是「点了没反应」—— 而从网关这一侧看，
// 「插件根本没发请求」和「发了但被拒」是两个完全不同的故障，此前两者都只表现为
// 「队列里没有新任务」。有了这行，一眼分开，不用猜。
//
// UA 也打出来：Zotero 的请求 UA 是可辨认的固定串，与面板及测试流量区分得开。
func (h *Handler) withQueueLog(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ua := r.Header.Get("User-Agent")
		if len(ua) > 60 {
			ua = ua[:60]
		}
		log.Printf("[queue] %s %s from %s ua=%q", r.Method, r.URL.Path, r.RemoteAddr, ua)
		next(w, r)
	}
}

// apiKeyOK 与 /v1/* 同口径（经 livecfg 读当前 key）。
func (h *Handler) apiKeyOK(r *http.Request) bool {
	want := h.loadLive().APIKey
	if want == "" {
		return false // 非回环 + 未配 key = 拒绝
	}
	got := strings.TrimSpace(strings.TrimPrefix(
		strings.TrimSpace(r.Header.Get("Authorization")), "Bearer "))
	return got == want
}

func (h *Handler) queue() *translate.Queue { return h.cfg.Translate }

// defaultModel 当前默认翻译模型；未配置时如实报 auto。
// 面板据此预选下拉框 —— 让「我上次选的」和「插件实际会用的」是同一个值。
func (h *Handler) defaultModel() string {
	if q := h.queue(); q != nil {
		if m := q.DefaultModel(); m != "" {
			return m
		}
	}
	return "auto"
}

// glossaryStatus 术语表体检结果，随列表下发。
//
// 为什么要在界面上显示：术语表是**可选**功能，配错了只会被忽略（不阻断翻译）。
// 于是「配了但没生效」和「配了且生效」在界面上长得一模一样 —— 用户会一直
// 以为术语在起作用。启动日志说过一次，但用户不会去看日志。
func (h *Handler) glossaryStatus() []translate.GlossaryCheck {
	q := h.queue()
	if q == nil {
		return []translate.GlossaryCheck{}
	}
	return translate.CheckGlossaries(q.GlossaryFiles())
}

// pdf2zhStatus pdf2zh 可执行文件的状态。队列不存在时如实返回不可用。
func (h *Handler) pdf2zhStatus() translate.Pdf2zhStatus {
	q := h.queue()
	if q == nil {
		return translate.Pdf2zhStatus{Error: "翻译未启用", Source: "config"}
	}
	return q.Pdf2zhStatus()
}

// writeJSONQ 只在队列端点用：与 /v1 的 OpenAI 错误体分开，因为调用方是插件，
// 它读的是 {"error": "..."} 这种平铺形状（见 LitFlow 契约）。
func writeJSONQ(w http.ResponseWriter, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		http.Error(w, `{"error":"encode failed"}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func writeErrQ(w http.ResponseWriter, status int, msg string) {
	writeJSONQ(w, status, map[string]any{"error": msg})
}

// health 统一探活。
//
// 同时带上 LitFlow 契约里的三个字段（status/ready/models）和本网关自己的
// （accounts/translate），因为两类调用方问的是不同的问题：
//   - 插件问「能提交吗」→ 看 ready
//   - 负载均衡/宿主问「健康吗」→ 看 status 码
//
// ready 的语义：模型目录已加载 **且** 翻译队列存在。这一条直接来自实测——
// 目录没加载时提交会被拒（否则 pdf2zh 拿到空链，第一个请求就失败）。
func (h *Handler) health(w http.ResponseWriter, r *http.Request) {
	total, healthy, _, _, _ := h.cfg.Pool.CountsDetailed()
	models := h.cachedModelCount()
	q := h.queue()
	translateOn := q != nil
	// ready 的判据是「能受理」，不是「目录已缓存」。
	//
	// 之前的写法把 models==0 也算不 ready，而目录缓存是懒加载的：进程刚起、
	// 还没人请求过 /v1/models 时它就是 0。于是插件探活会看到一个「不 ready」
	// 的健康网关，然后退到脚本回退——一个假阴性，而且只在启动后的头几秒出现，
	// 是最难复现的那种。
	//
	// 真正的受理条件是：翻译队列在 + 至少一个账号此刻可服务。
	ready := translateOn && h.cfg.Pool.ServableNow()
	var jobs map[string]int
	if q != nil {
		jobs = q.Counts()
	}
	writeJSONQ(w, http.StatusOK, map[string]any{
		"status":   "ok",
		"ready":    ready,
		"models":   models,
		"accounts": map[string]any{"total": total, "healthy": healthy},
		"translate": map[string]any{
			"enabled": translateOn,
			"jobs":    jobs,
			"chain":   h.cfg.TranslateChain,
		},
	})
}

// cachedModelCount 只读缓存，不打上游。
func (h *Handler) cachedModelCount() int {
	return len(cachedModelsSnapshot())
}

// translateList 列表 + 计数 + 轮换链。
func (h *Handler) translateList(w http.ResponseWriter, r *http.Request) {
	q := h.queue()
	if q == nil {
		writeJSONQ(w, http.StatusOK, map[string]any{
			"configured": false, "jobs": []any{}, "counts": map[string]int{},
			"chain": h.cfg.TranslateChain})
		return
	}
	jobs := q.List(50)
	out := make([]any, 0, len(jobs))
	for _, j := range jobs {
		out = append(out, translate.MarshalJob(j))
	}
	writeJSONQ(w, http.StatusOK, map[string]any{
		"configured":    true,
		"counts":        q.Counts(),
		"jobs":          out,
		"chain":         h.cfg.TranslateChain,
		"default_model": h.defaultModel(),
		// 术语表状态随列表下发。它是可选功能，配错了只会被忽略 —— 不在这里
		// 显示出来，用户就永远不知道「我配了术语表」到底生效没有。
		"glossaries": h.glossaryStatus(),
		// pdf2zh 是否可用。新装用户最容易卡这里，而失败要等到第一次提交
		// 才暴露；提前一句话能省掉一整轮。
		"pdf2zh": h.pdf2zhStatus(),
	})
}

// translateSubmit 提交。两种形态：JSON 给路径，multipart 上传文件。
func (h *Handler) translateSubmit(w http.ResponseWriter, r *http.Request) {
	q := h.queue()
	if q == nil {
		writeErrQ(w, http.StatusNotImplemented,
			"translation is not enabled (config.json translate.enabled)")
		return
	}
	if strings.HasPrefix(strings.ToLower(r.Header.Get("Content-Type")),
		"multipart/form-data") {
		h.translateUpload(w, r, q)
		return
	}

	var body struct {
		Source       string `json:"source"`
		OutputDir    string `json:"output_dir"`
		Model        string `json:"model"`
		QPS          string `json:"qps"`
		SkipExisting *bool  `json:"skip_existing"`
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, queueJSONLimit))
	if err != nil {
		writeErrQ(w, http.StatusBadRequest, "read body: "+err.Error())
		return
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &body); err != nil {
			writeErrQ(w, http.StatusBadRequest, "invalid json: "+err.Error())
			return
		}
	}
	if strings.TrimSpace(body.Source) == "" {
		writeErrQ(w, http.StatusBadRequest,
			`source (an absolute path to a .pdf) is required, or upload a file as multipart "file"`)
		return
	}
	skip := true
	if body.SkipExisting != nil {
		skip = *body.SkipExisting
	}
	job, err := q.Submit(body.Source, body.OutputDir, body.Model, body.QPS, skip)
	if err != nil {
		writeErrQ(w, http.StatusBadRequest, err.Error())
		return
	}
	writeSubmitResult(w, job)
}

// writeSubmitResult 把 Submit 的结果映射成 HTTP 响应。
//
// 202 的语义是「接下了、还没做完」。而 Submit 成功有三种，只有一种是「接下了」：
//
//	queued  真接下了        → 202
//	deduped 返回队列里原来那个，本次没新增工作 → 200
//	skipped 磁盘上已有译文，什么都没排队      → 200
//
// 旧代码一律回 202，于是 skipped 也在说「接下了」—— 调用方会去轮询一个**已经是
// 终态**的任务。这不是理论问题：跳过是「已有译文」的常见路径，而轮询循环里
// 没有任何东西会让它推进。
//
// outcome 也一并写进响应体，让调用方不必从 job.state 反推「刚才到底发生了什么」。
func writeSubmitResult(w http.ResponseWriter, res translate.SubmitResult) {
	status := http.StatusAccepted
	if res.Outcome != translate.SubmitQueued {
		status = http.StatusOK
	}
	body := translate.MarshalJob(res.Job)
	body["outcome"] = string(res.Outcome)
	writeJSONQ(w, status, body)
}

// translateUpload 处理 multipart：把文件落盘再入队。
func (h *Handler) translateUpload(w http.ResponseWriter, r *http.Request,
	q *translate.Queue) {
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		writeErrQ(w, http.StatusBadRequest, "parse form: "+err.Error())
		return
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		writeErrQ(w, http.StatusBadRequest, `missing file field: `+err.Error())
		return
	}
	defer file.Close()

	name := filepath.Base(header.Filename)
	if !strings.EqualFold(filepath.Ext(name), ".pdf") {
		writeErrQ(w, http.StatusBadRequest, "only .pdf files are accepted")
		return
	}

	dir := h.cfg.TranslateWorkDir
	if dir == "" {
		dir = filepath.Join(os.TempDir(), "wb2api-translate")
	}
	uploadDir := filepath.Join(dir, "uploads")
	if err := os.MkdirAll(uploadDir, 0o755); err != nil {
		writeErrQ(w, http.StatusInternalServerError, "cannot create upload dir: "+err.Error())
		return
	}
	// 同名覆盖：重复上传同一篇应该替换而不是堆出 (1)(2)(3)。
	dst := filepath.Join(uploadDir, name)
	out, err := os.Create(dst)
	if err != nil {
		writeErrQ(w, http.StatusInternalServerError, "cannot write upload: "+err.Error())
		return
	}
	if _, err := io.Copy(out, file); err != nil {
		out.Close()
		writeErrQ(w, http.StatusInternalServerError, "upload failed: "+err.Error())
		return
	}
	out.Close()

	job, err := q.Submit(dst, dir, r.FormValue("model"), r.FormValue("qps"), true)
	if err != nil {
		writeErrQ(w, http.StatusBadRequest, err.Error())
		return
	}
	writeSubmitResult(w, job)
}

func (h *Handler) translateGetRoute(w http.ResponseWriter, r *http.Request) {
	q := h.queue()
	if q == nil {
		writeErrQ(w, http.StatusNotImplemented, "translation is not enabled")
		return
	}
	id := r.PathValue("id")
	job := q.Get(id)
	if job == nil {
		writeErrQ(w, http.StatusNotFound, "no such job: "+id)
		return
	}
	writeJSONQ(w, http.StatusOK, translate.MarshalJob(job))
}

func (h *Handler) translateCancelRoute(w http.ResponseWriter, r *http.Request) {
	q := h.queue()
	if q == nil {
		writeErrQ(w, http.StatusNotImplemented, "translation is not enabled")
		return
	}
	id := r.PathValue("id")
	if !q.Cancel(id) {
		writeErrQ(w, http.StatusConflict,
			"job is not cancellable (already finished, or unknown)")
		return
	}
	writeJSONQ(w, http.StatusOK, map[string]any{"ok": true, "id": id})
}

// mustWorkDir 供日志用：把实际产出目录报出来。
func (h *Handler) TranslateWorkDirOrTemp() string {
	if h.cfg.TranslateWorkDir != "" {
		return h.cfg.TranslateWorkDir
	}
	return filepath.Join(os.TempDir(), "wb2api-translate")
}
