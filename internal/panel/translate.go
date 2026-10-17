package panel

// 翻译面板接口。
//
// 四个端点，形状对齐已有的账号接口（同一套 Bearer 鉴权、同一个 writeJSON/
// writeErr）：翻译不是面板里的异类，它和「签到」「余额」一样是面板的一个动作。
//
//	POST /panel/api/translate              上传或按路径提交一个 PDF
//	GET  /panel/api/translate              任务列表 + 状态计数
//	GET  /panel/api/translate/{id}         单任务详情
//	POST /panel/api/translate/{id}/cancel  取消
//
// 刻意**没有**进度百分比：pdf2zh 用 rich 画阶段条，管线下不吐（实测 7 种环境
// 变量组合全部无效）。编一个会动的条就是在撒谎，所以只给 elapsed 和 pdf2zh
// 自报的 token 总量。

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/linguo2625469/workbuddy2api-panel/internal/translate"
)

// translateQueue 取队列；未配置时 nil。
func (p *Panel) translateQueue() *translate.Queue { return p.cfg.Translate }

// readJSON 解一个 JSON body。
//
// 限长 1 MiB：这些端点收的都是路径/参数，不是文件内容（文件走 multipart）。
// 不设上限的话，一个手滑把 PDF 当 JSON POST 过来就会把内存吃满。
func readJSON(r *http.Request, v any) error {
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return err
	}
	if len(raw) == 0 {
		return errors.New("empty body")
	}
	return json.Unmarshal(raw, v)
}

// translateSubmit 提交一个 PDF。
//
// 两种来源，都支持：
//
//	Content-Type: application/json      {"source": "D:\\paper.pdf", ...}
//	Content-Type: multipart/form-data   file=<pdf>
//
// 两种都要，因为用途不同：本机自己有文件时给路径最省事（不用先上传再写盘）；
// 从浏览器另一个机器访问时只能上传。
func (p *Panel) translateSubmit(w http.ResponseWriter, r *http.Request) {
	q := p.translateQueue()
	if q == nil {
		writeErr(w, http.StatusNotImplemented,
			"translation is not configured in this build/config")
		return
	}

	ct := strings.ToLower(r.Header.Get("Content-Type"))
	if strings.HasPrefix(ct, "multipart/form-data") {
		p.translateSubmitUpload(w, r, q)
		return
	}

	var body struct {
		Source       string `json:"source"`
		OutputDir    string `json:"output_dir"`
		Model        string `json:"model"`
		QPS          string `json:"qps"`
		SkipExisting *bool  `json:"skip_existing"`
	}
	if err := readJSON(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	if strings.TrimSpace(body.Source) == "" {
		writeErr(w, http.StatusBadRequest,
			"source (an absolute path to a .pdf) is required, or upload a file")
		return
	}
	skip := true
	if body.SkipExisting != nil {
		skip = *body.SkipExisting
	}
	job, err := q.Submit(body.Source, body.OutputDir, body.Model, body.QPS, skip)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	// 202：任务被接收但还没跑完。用 200 会让调用方以为可以直接读产出。
	writeJSON(w, http.StatusAccepted, translate.MarshalJob(job))
}

// translateSubmitUpload 处理 multipart 上传。
func (p *Panel) translateSubmitUpload(w http.ResponseWriter, r *http.Request,
	q *translate.Queue) {
	// 与 import.go 同一口径：32 MiB 够一份论文，超过就不是本功能的输入。
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		writeErr(w, http.StatusBadRequest, "parse form: "+err.Error())
		return
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		writeErr(w, http.StatusBadRequest, "missing file field: "+err.Error())
		return
	}
	defer file.Close()

	name := filepath.Base(header.Filename)
	if !strings.EqualFold(filepath.Ext(name), ".pdf") {
		writeErr(w, http.StatusBadRequest, "only .pdf files are accepted")
		return
	}

	// 上传的落盘位置：<work_dir>/uploads。放在 work_dir 而不是系统临时目录，
	// 因为产出要和输入放在一起，用户才找得到自己的双语 PDF。
	dir := p.cfg.TranslateWorkDir
	if dir == "" {
		dir = filepath.Join(os.TempDir(), "wb2api-translate")
	}
	uploadDir := filepath.Join(dir, "uploads")
	if err := os.MkdirAll(uploadDir, 0o755); err != nil {
		writeErr(w, http.StatusInternalServerError,
			"cannot create upload dir: "+err.Error())
		return
	}
	dst := filepath.Join(uploadDir, name)
	out, err := os.Create(dst)
	if err != nil {
		writeErr(w, http.StatusInternalServerError,
			"cannot write upload: "+err.Error())
		return
	}
	if _, err := io.Copy(out, file); err != nil {
		out.Close()
		writeErr(w, http.StatusInternalServerError, "upload failed: "+err.Error())
		return
	}
	out.Close()

	job, err := q.Submit(dst, dir, r.FormValue("model"), r.FormValue("qps"), true)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, translate.MarshalJob(job))
}

// translateList 任务列表与计数。
func (p *Panel) translateList(w http.ResponseWriter, r *http.Request) {
	q := p.translateQueue()
	if q == nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok": false, "configured": false, "jobs": []any{},
			"counts": map[string]int{}, "chain": p.cfg.TranslateChain})
		return
	}
	jobs := q.List(50)
	out := make([]any, 0, len(jobs))
	for _, j := range jobs {
		out = append(out, translate.MarshalJob(j))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":         true,
		"configured": true,
		"counts":     q.Counts(),
		"jobs":       out,
		"chain":      p.cfg.TranslateChain,
	})
}

// translateGet 单任务。
func (p *Panel) translateGet(w http.ResponseWriter, r *http.Request, id string) {
	q := p.translateQueue()
	if q == nil {
		writeErr(w, http.StatusNotImplemented, "translation is not configured")
		return
	}
	job := q.Get(id)
	if job == nil {
		writeErr(w, http.StatusNotFound, "no such job: "+id)
		return
	}
	writeJSON(w, http.StatusOK, translate.MarshalJob(job))
}

// translateCancel 取消。
func (p *Panel) translateCancel(w http.ResponseWriter, r *http.Request, id string) {
	q := p.translateQueue()
	if q == nil {
		writeErr(w, http.StatusNotImplemented, "translation is not configured")
		return
	}
	if !q.Cancel(id) {
		writeErr(w, http.StatusConflict,
			"job is not cancellable (already finished, or unknown)")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": id})
}

// TranslateWorkDir 产出目录，空则让调用方决定。
func (p *Panel) TranslateWorkDir() string { return p.cfg.TranslateWorkDir }

// translateGetRoute / translateCancelRoute 处理带 {id} 的路由。
//
// 单独两个薄包装而不是把 id 解析塞进 handler：Go 1.22 的 ServeMux 已经给出
// PathValue，任何第二种解析方式（切字符串、正则）都会和它悄悄不一致。
func (p *Panel) translateGetRoute(w http.ResponseWriter, r *http.Request) {
	p.translateGet(w, r, r.PathValue("id"))
}

func (p *Panel) translateCancelRoute(w http.ResponseWriter, r *http.Request) {
	p.translateCancel(w, r, r.PathValue("id"))
}