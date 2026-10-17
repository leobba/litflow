// Package translate 把 PDF 翻译接进网关本体。
//
// 背景：翻译此前是另一个 Python 程序，用户要开三个终端。把它放进这个二进制
// 的意义不是「少一个窗口」，而是**翻译的每一次静默失败都被同一套观测接住了**
// ——上游的 429、账号的 6004、pdf2zh 的 0 token，此后都在同一个面板上。
//
// 设计上的三条硬边界：
//
//  1. **pdf2zh_next 永远是子进程。** 它是 AGPL v3；只 spawn 不链接，是聚合
//     而非衍生。这条边界不能因为「合体」而破——破了下游分发就受 AGPL 传染。
//  2. **队列串行。** 实测两个 BabelDOC 进程各约 1.8 GB 常驻，并发跑就是把
//     翻译变成 swap 风暴。串行不是保守，是这个机器的物理约束。
//  3. **不编造进度。** pdf2zh 用 rich 画表，管线下不吐阶段条（实测：7 种环境
//     变量组合全部无效）。所以这里只给 elapsed 和 pdf2zh 自己报的 token 总量，
//     不给百分比。
//
// 下面每一条常量后面都跟着一个实测结论。那些不是注释风格，是**别把踩过的坑
// 再踩一次**的记录。
package translate

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
)

// PlaceholderKey 交给 pdf2zh 的占位 key。
//
// pdf2zh 在 validate_settings() 里校验——没有 api_key 它**拒绝启动**，是死
// 进程而不是更安全的进程。本地网关根本不验它，所以给一个固定的假值。
const PlaceholderKey = "wb2api-local"

// DefaultQPS 是 pdf2zh 的 --qps，也顺带决定 worker 池大小。
//
// 实测：qps 3、跨两份文档，就足以撞出 429 风暴。免费池按**会话**限流，
// 调高 qps 不会更快，只会更早撞墙且并行撞。
const DefaultQPS = "2"

// ClientTimeoutS 是交给 pdf2zh 的 --openai-compatible-timeout。
//
// 必须**大于**网关这一侧的最坏应答时间，否则 pdf2zh 会在中途放弃——而放弃
// 请求时 BabelDOC 用原文替换该段且不报错，那是本项目存在的理由本身。
const ClientTimeoutS = "120"

// serialQueueBodyRe 从 pdf2zh 自己的输出里取 token 总量。
//
// 这是唯一 stdlib 可读的「到底翻没翻」信号：Total 0 意味着 BabelDOC 把每段
// 都换成了原文，文件是真的、退出码是 0、内容没译。
var serialQueueBodyRe = regexp.MustCompile(`(?i)Total Token Usage:\s*Total\s*(\d+)`)

// Job 一个 PDF 的翻译尝试。
type Job struct {
	ID        string  `json:"id"`
	Source    string  `json:"source"`
	OutputDir string  `json:"output_dir"`
	Model     string  `json:"model"`
	QPS       string  `json:"qps"`
	State     string  `json:"state"`
	// FailKind 失败类别（failure.go 的常量）。空 = 未分类。
	FailKind string `json:"fail_kind,omitempty"`
	CreatedAt float64 `json:"created_at"`
	StartedAt float64 `json:"started_at"`
	FinishedAt float64 `json:"finished_at"`

	// DualPDFs 产出的双语文件（绝对路径）。
	DualPDFs []string `json:"dual_pdfs"`

	// Error 失败原因。
	Error string `json:"error"`

	// ProducedNothing 退出码 0 但没有产出双语文件 —— **静默失败**。
	// 它单独一个字段而不是塞进 Error：读起来像崩溃会掩盖「看起来成功」。
	ProducedNothing bool `json:"produced_nothing"`

	// Partial 有产出但模型几乎没跑（token 总量为 0）——**另一个静默失败**。
	// 实测：10 页论文回来是真的双语 PDF、退出码 0、1.3% 中文，四页一个字没有。
	Partial bool   `json:"partial"`
	Warning string `json:"warning"`

	// OutputTail pdf2zh 输出的尾部。
	//
	// 没有它，「完成但没产出」这个状态无法诊断——只报结果不报原因，唯一剩下
	// 的动作是手工重跑碰运气。实测代价：51 个失败任务加一次长时间误诊。
	OutputTail string `json:"output_tail"`

	// 本次尝试用到的 token 总量（pdf2zh 自报，未知为 -1）。
	TokenTotal int64 `json:"token_total"`
}

// ElapsedS 已用秒数，给界面显示「在动」而不是「卡住」。
func (j *Job) ElapsedS() float64 {
	end := j.FinishedAt
	if end == 0 {
		end = float64(time.Now().UnixNano()) / 1e9
	}
	start := j.StartedAt
	if start == 0 {
		start = j.CreatedAt
	}
	if end <= start {
		return 0
	}
	return end - start
}

// Liveness 运行中的一句话。**不是百分比** —— 见包注释第 3 条。
func (j *Job) Liveness() string {
	switch j.State {
	case StateRunning:
		return fmt.Sprintf("running %.0fs", j.ElapsedS())
	case StateQueued:
		return "queued"
	case StateSkipped:
		return "already translated"
	case StateDone:
		return fmt.Sprintf("done in %.0fs", j.ElapsedS())
	default:
		return j.State
	}
}

const (
	StateQueued    = "queued"
	StateRunning   = "running"
	StateDone      = "done"
	StateFailed    = "failed"
	StateCancelled = "cancelled"
	StateSkipped   = "skipped"
)

func terminal(state string) bool {
	switch state {
	case StateDone, StateFailed, StateCancelled, StateSkipped:
		return true
	}
	return false
}

// Config 翻译子系统的配置。全部有安全默认值——一个刚装好的程序不该因为
// 少配一个字段就不能翻译。
type Config struct {
	// Enabled 关掉后 Submit 返回错误，不静默吞。
	Enabled bool `json:"enabled"`

	// PDF2zhExe pdf2zh_next 的绝对路径。空 = 从 PATH 找。
	PDF2zhExe string `json:"pdf2zh_exe"`

	// WorkDir 产出与中间文件落在哪。空 = PDF 同目录。
	WorkDir string `json:"work_dir"`

	// Glossaries 用户术语表（CSV 路径列表，逗号分隔传给 pdf2zh）。
	//
	// 与 AutoGlossary 是两件事：那个是自动抽取（第二遍全文档 LLM 调用，会 429），
	// 这个是静态文件、零额外请求。默认关掉自动抽取，但这一项可以放心开。
	Glossaries []string `json:"glossaries"`

	// DefaultModel 提交时没带模型用哪个。**不要硬编码 auto**：
	// Zotero 插件只发 {source}，不带模型 —— 所以这条默认值就是它实际使用的模型。
	// auto 是网关里唯一不公布倍率、且文档明说会浮动的模型，把它当默认等于
	// 让「用户在面板里选的模型」永远不生效。实测：用户选了 hy3，请求日志里
	// 全是 auto。
	DefaultModel string `json:"default_model"`
	LangIn  string `json:"lang_in"`
	LangOut string `json:"lang_out"`
	QPS     string `json:"qps"`

	// IgnoreCache 传给 pdf2zh 的 --ignore-cache。
	//
	// 默认 false（重跑快），但**测量必须打开**：pdf2zh 的
	// _translationcache 键是「引擎+参数+原文」，修好一个模型再跑会拿到旧译文，
	// 任何 A/B 都变成「一次运行和它自己比」。实测两次不同上游跑出
	// byte-identical 文本，就是这么发现的。
	IgnoreCache bool `json:"ignore_cache"`

	// AutoGlossary 术语自动抽取。默认 false。
	//
	// 这不是质量/速度偏好，是免费池能不能活下来的问题：打开会多一趟全文档
	// LLM 调用，请求量翻倍，在按会话限流的池子上就是 429。实测：51 个连续
	// 失败任务，全程伴随 automatic_term_extractor 警告，直到关掉才停。
	AutoGlossary bool `json:"auto_glossary"`

	// TimeoutS 单个 job 的总时限（不是单请求）。
	TimeoutS int `json:"timeout_s"`

	// MaxHistory 保留多少个历史任务。
	MaxHistory int `json:"max_history"`
}

// DefaultConfig 安全的默认值。
func DefaultConfig() Config {
	return Config{
		Enabled:    true,
		LangIn:     "en",
		LangOut:    "zh-CN",
		QPS:        DefaultQPS,
		TimeoutS:   3600,
		MaxHistory: 200,
	}
}

// Queue 串行翻译队列，拥有自己的 worker goroutine。
type Queue struct {
	mu    sync.RWMutex
	cfg   Config
	jobs  map[string]*Job
	order []string
	// running 在飞进程，让 Cancel 能真停掉它而不是只标记。
	running map[string]*exec.Cmd

	// persistPath 历史任务落盘文件。空 = 只活在这个进程里。
	//
	// 队列原本纯内存，一次重启就抹掉全部在飞与历史任务。对「300 页的书跑
	// 一小时」这种用法是硬伤：重启后任务消失，而插件还在轮询一个不存在的 id。
	persistPath string

	pending chan string
	stop    chan struct{}
	wg      sync.WaitGroup

	// onChange 状态变化回调，供面板推送/日志用。
	onChange func(*Job)

	// baseURL 这个网关自己的地址：pdf2zh 要指回我们。
	baseURL string
	apiKey  string

	startOnce sync.Once
	stopOnce  sync.Once
}

// NewQueue 建队列。baseURL 形如 http://127.0.0.1:7863/v1。
func NewQueue(cfg Config, baseURL, apiKey string) *Queue {
	return &Queue{
		cfg:     cfg,
		jobs:    make(map[string]*Job),
		pending: make(chan string, 1024),
		stop:    make(chan struct{}),
		baseURL: baseURL,
		apiKey:  apiKey,
	}
}

// SetDefaultModel 运行期改默认模型（面板保存时调用，无需重启）。
func (q *Queue) SetDefaultModel(m string) {
	q.mu.Lock()
	q.cfg.DefaultModel = m
	q.mu.Unlock()
}

// DefaultModel 当前默认模型。
// GlossaryFiles 当前配置的术语表路径（副本，调用方不能改内部状态）。
//
// 从队列而不是从 Handler 的 cfg 读：面板保存设置走的是 Queue.SetGlossaries，
// 从 cfg 读会拿到启动时的旧值，表现为「保存了但界面不变」。
func (q *Queue) GlossaryFiles() []string {
	q.mu.RLock()
	defer q.mu.RUnlock()
	return append([]string(nil), q.cfg.Glossaries...)
}

// SetGlossaries 更新术语表路径（面板保存时调用）。
func (q *Queue) SetGlossaries(paths []string) {
	q.mu.Lock()
	q.cfg.Glossaries = append([]string(nil), paths...)
	q.mu.Unlock()
	q.changed(nil)
}
func (q *Queue) DefaultModel() string {
	q.mu.RLock()
	defer q.mu.RUnlock()
	return q.cfg.DefaultModel
}

// SetOnChange 状态变化回调。
func (q *Queue) SetOnChange(fn func(*Job)) { q.onChange = fn }

// Start 起 worker。幂等。
func (q *Queue) Start() {
	q.startOnce.Do(func() {
		q.wg.Add(1)
		go q.loop()
	})
}

// Stop 停 worker，等当前 job 收尾。
func (q *Queue) Stop() {
	q.stopOnce.Do(func() { close(q.stop) })
	q.wg.Wait()
}

func (q *Queue) loop() {
	defer q.wg.Done()
	for {
		select {
		case <-q.stop:
			return
		case id := <-q.pending:
			q.runOne(id)
		}
	}
}

// SubmitOutcome Submit 成功时的三种不同结果。
//
// 为什么要显式区分：这三种都返回 (job, nil)，调用方只能靠 job.State 反推自己
// 发生了什么，而 State 是**任务的状态**、不是**这次调用的结果**。区别很实在：
//
//	queued  真的接下了活，接下来会有人跑它
//	deduped 同源已在队列里，返回的是**那一个** —— 本次没有新增工作
//	skipped 磁盘上已有译文，什么都没排队
//
// 旧代码把三种一律回 202 Accepted。202 的意思是「接下了、还没做完」，而 skipped
// 恰恰是**什么都没接**。调用方据此以为排上了队，就会去轮询一个永远不会推进的
// 任务 —— 它已经是终态了。
type SubmitOutcome string

const (
	SubmitQueued  SubmitOutcome = "queued"
	SubmitDeduped SubmitOutcome = "deduped"
	SubmitSkipped SubmitOutcome = "skipped"
)

// SubmitResult 一次 Submit 的完整结果。
type SubmitResult struct {
	Job     *Job
	Outcome SubmitOutcome
}
// Submit 排队一个 PDF。已在队列里（queued/running）的同一路径返回原 job，
// 不产生第二个——Zotero 的通知会对同一次导入触发多次，重复排队等于付费翻两次。
func (q *Queue) Submit(source, outputDir, model, qps string, skipExisting bool) (SubmitResult, error) {
	if !q.cfg.Enabled {
		return SubmitResult{}, fmt.Errorf("translation is disabled in config")
	}
	src, err := filepath.Abs(source)
	if err != nil {
		return SubmitResult{}, err
	}
	if st, err := os.Stat(src); err != nil || st.IsDir() {
		return SubmitResult{}, fmt.Errorf("source not found: %s", src)
	}
	if strings.ToLower(filepath.Ext(src)) != ".pdf" {
		return SubmitResult{}, fmt.Errorf("not a PDF: %s", filepath.Base(src))
	}
	if outputDir == "" {
		// **产出默认落在源 PDF 自己旁边**，而不是 WorkDir。
		//
		// 这条决定的是「翻译完之后 Zotero 能不能自动挂上附件」：插件的
		// syncOnce() 是在**条目自己的 storage 目录**里找 *dual.pdf。源文件
		// 就在那个目录里，所以双语的落点必须和它一致。写到一个统一的
		// work_dir 里会让插件永远找不到，表现为「翻译成功了但 Zotero 里没附件」。
		//
		// WorkDir 仍然是上传件的落点（调用方显式传入），因为上传件没有
		// 天然的归属目录。
		outputDir = filepath.Dir(src)
	}
	if outputDir, err = filepath.Abs(outputDir); err != nil {
		return SubmitResult{}, err
	}
	// 解析成**实际会用的**模型，而不是占位的 "auto"。
	//
	// 这里曾经写死 "auto"，于是 buildArgs 里读 DefaultModel 的分支永远走不到 ——
	// 死代码。表现是：面板里选了 hy3、接口也返回 default_model=cn:hy3，
	// 而插件提交的任务实际仍然打 auto。**两处都"正常"，中间的链路断了。**
	// 顺带：job.Model 存的就是真正会用的模型，面板显示的和实际发出的一致。
	if model == "" {
		model = q.cfg.DefaultModel
		if model == "" {
			model = "auto"
		}
	}
	if qps == "" {
		qps = q.cfg.QPS
		if qps == "" {
			qps = DefaultQPS
		}
	}

	q.mu.Lock()
	// 同路径在飞行中直接返回。
	for _, j := range q.jobs {
		if j.Source == src && (j.State == StateQueued || j.State == StateRunning) {
			q.mu.Unlock()
			return SubmitResult{Job: j, Outcome: SubmitDeduped}, nil
		}
	}
	job := &Job{
		ID:        newID(),
		Source:    src,
		OutputDir: outputDir,
		Model:     model,
		QPS:       qps,
		State:     StateQueued,
		CreatedAt: nowS(),
		TokenTotal: -1,
	}
	// 已有产出就跳过：不是失败，但也不能什么都不说。
	if skipExisting {
		if existing := findDualPDFs(outputDir, src); len(existing) > 0 {
			job.State = StateSkipped
			job.DualPDFs = existing
			job.FinishedAt = nowS()
			job.Error = "dual already present"
			q.rememberLocked(job)
			q.mu.Unlock()
			q.changed(job)
			return SubmitResult{Job: job, Outcome: SubmitSkipped}, nil
		}
	}
	q.rememberLocked(job)
	q.mu.Unlock()
	q.changed(job)

	q.saveAsync()
	select {
	case q.pending <- job.ID:
	case <-q.stop:
		return SubmitResult{}, fmt.Errorf("queue is shutting down")
	}
	return SubmitResult{Job: job, Outcome: SubmitQueued}, nil
}

// Get 按 id 取。**返回副本**，避免调用方与 worker 竞争。
func (q *Queue) Get(id string) *Job {
	q.mu.RLock()
	defer q.mu.RUnlock()
	j, ok := q.jobs[id]
	if !ok {
		return nil
	}
	cp := *j
	cp.DualPDFs = append([]string(nil), j.DualPDFs...)
	return &cp
}

// List 最近的任务，新的在前。
func (q *Queue) List(limit int) []*Job {
	q.mu.RLock()
	defer q.mu.RUnlock()
	if limit <= 0 || limit > len(q.order) {
		limit = len(q.order)
	}
	out := make([]*Job, 0, limit)
	for i := len(q.order) - 1; i >= 0 && len(out) < limit; i-- {
		if j, ok := q.jobs[q.order[i]]; ok {
			cp := *j
			cp.DualPDFs = append([]string(nil), j.DualPDFs...)
			out = append(out, &cp)
		}
	}
	return out
}

// Counts 各状态计数，给面板徽标。
func (q *Queue) Counts() map[string]int {
	q.mu.RLock()
	defer q.mu.RUnlock()
	out := map[string]int{}
	for _, j := range q.jobs {
		out[j.State]++
	}
	return out
}

// Cancel 取消 job；运行中的会真的杀掉子进程。
//
// 锁的纪律与 finish 相同：**只在一个来源里加解锁**。这里原来是
// `defer Unlock` 加中途一对显式 Unlock/Lock，虽然当时是配平的，但那种写法
// 只要插入一行 return 就会双重解锁，而双重解锁是 fatal error（直接杀进程，
// 不是 panic）。所以改成：锁内只做状态决策，锁外回调。
func (q *Queue) Cancel(id string) bool {
	var changed bool
	var cmd *exec.Cmd

	q.mu.Lock()
	j, ok := q.jobs[id]
	if !ok || terminal(j.State) {
		q.mu.Unlock()
		return false
	}
	if j.State == StateQueued {
		j.State = StateCancelled
		j.FinishedAt = nowS()
		changed = true
	} else {
		// 运行中：标记 + **真的杀进程**。只标记会让串行队列陪它跑完，
		// 用户以为取消了但队列一小时不动。
		j.State = StateCancelled
		cmd = q.running[id]
	}
	q.mu.Unlock()

	if changed {
		q.changed(j)
	}
	q.saveAsync()
	if cmd != nil && cmd.Process != nil {
		// 在 goroutine 里杀：Kill 会等进程退出，不该拖住 HTTP 处理。
		go func() { _ = cmd.Process.Kill() }()
	}
	return true
}

func (q *Queue) rememberLocked(job *Job) {
	q.jobs[job.ID] = job
	q.order = append(q.order, job.ID)
	q.trimLocked()
}

// trimLocked 只从**已终止**的任务里淘汰，在飞任务永不丢。
func (q *Queue) trimLocked() {
	max := q.cfg.MaxHistory
	if max <= 0 {
		max = 200
	}
	for len(q.order) > max {
		removed := false
		for i, id := range q.order {
			if j, ok := q.jobs[id]; ok && terminal(j.State) {
				q.order = append(q.order[:i], q.order[i+1:]...)
				delete(q.jobs, id)
				removed = true
				break
			}
		}
		if !removed {
			break // 剩下的全在飞，拒绝淘汰
		}
	}
}

func (q *Queue) changed(job *Job) {
	if q.onChange != nil {
		cp := *job
		cp.DualPDFs = append([]string(nil), job.DualPDFs...)
		func() {
			defer func() { _ = recover() }() // 观察者绝不能让队列崩
			q.onChange(&cp)
		}()
	}
}

// runOne 跑一个 job。所有失败都要落成**可读的状态**，不能只剩一个空 job。
func (q *Queue) runOne(id string) {
	q.mu.Lock()
	job, ok := q.jobs[id]
	if !ok || job.State != StateQueued {
		q.mu.Unlock()
		return
	}
	job.State = StateRunning
	job.StartedAt = nowS()
	q.mu.Unlock()
	q.changed(job)

	// 先算一次是否已被取消（Submit → 入队之间的竞态）。
	q.mu.RLock()
	cancelledBeforeStart := job.State == StateCancelled
	q.mu.RUnlock()
	if cancelledBeforeStart {
		q.finish(job, StateCancelled, "", nil, -1, "")
		return
	}

	exitCode, output, duals, err := q.spawn(job)
	if err != nil {
		kind, _ := ClassifyFailure(err.Error())
		q.finish(job, StateFailed, err.Error(), nil, -1, kind)
		return
	}

	tokens := parseTokenTotal(output)
	switch {
	case exitCode != 0:
		last := lastLine(output)
		// 按**整段输出**分类，而不是按 lastLine：429 之类往往不在最后一行，
		// 最后一行常是重试日志。建议文案由 finish 统一追加（FailureHint）。
		kind, _ := ClassifyFailure(output)
		msg := fmt.Sprintf("pdf2zh exit %d: %s", exitCode, last)
		if kind == FailUnknown {
			// 认不出就明说，而不是装作认得。原始输出已在 OutputTail 里。
			msg += "（无法从输出判断原因，原始输出见任务详情）"
		}
		q.finish(job, StateFailed, msg, duals, tokens, kind)
	case len(duals) == 0:
		// **静默失败**：进程成功结束，但没有双语文件。
		q.finish(job, StateFailed,
			"pdf2zh finished but produced no dual PDF — the output would be "+
				"missing paragraphs silently", nil, tokens, "")
	default:
		q.finish(job, StateDone, "", duals, tokens, "")
	}
}

// finish 落终态并判定静默失败。
//
// 锁的纪律：整段修改在锁内完成，**然后释放**，再调回调。
// 这里曾经同时写了 `defer q.mu.Unlock()` 和一句显式 `q.mu.Unlock()`，
// 结果第二次解锁被 Go 判成 `fatal error: sync: Unlock of unlocked RWMutex`
// ——那不是 panic，是**直接杀掉进程**，网关连同所有账号池状态一起消失。
// 教训：RWMutex 的加减锁只能有一个来源，要么 defer 要么显式，不能都写。
func (q *Queue) finish(job *Job, state, msg string, duals []string, tokens int64, kind string) {
	// kind 由调用方按**原始输出**分类得到（failure.go）。放在参数里而不是在
	// 这里猜，是因为 finish 只看得到一句话，看不到整段输出。
	q.mu.Lock()

	// 取消标记优先：用户在运行中点过取消。
	if state != StateCancelled && job.State == StateCancelled {
		state = StateCancelled
		msg = ""
	}
	job.State = state
	job.FinishedAt = nowS()
	job.TokenTotal = tokens
	if len(duals) > 0 {
		job.DualPDFs = duals
	}
	job.Error = msg
	job.FailKind = kind
	// 失败但分类不出来时，建议文案留空 —— 不给一个自信的错误诊断。
	if kind != "" {
		if hint := FailureHint(kind); hint != "" && !strings.Contains(msg, hint) {
			job.Error = strings.TrimSpace(msg + "\n" + hint)
		}
	}

	switch {
	case state == StateFailed && len(duals) == 0 && strings.Contains(msg, "produced no dual"):
		job.ProducedNothing = true
	case state == StateDone && tokens == 0:
		// 有双语文件、退出码 0、每个更早的检查都说成功。剩下一个问题：
		// 模型到底跑了吗？pdf2zh 自报 token 总量，0 就是答案。
		job.Partial = true
		job.Warning = "a dual PDF was produced but pdf2zh used 0 tokens, so the " +
			"model never ran: most paragraphs are the ORIGINAL text. The file " +
			"looks finished and is largely untranslated."
	}
	q.trimLocked()
	q.mu.Unlock()

	// 回调在锁外：观察者可能反过来查队列，持锁调用就会自锁。
	q.changed(job)
	// 终态必须落盘：这是历史记录唯一有价值的时刻。
	q.saveAsync()
}

// spawn 组命令行并跑 pdf2zh，保留输出尾部。
func (q *Queue) spawn(job *Job) (int, string, []string, error) {
	exe, err := q.resolveExe()
	if err != nil {
		return 0, "", nil, err
	}
	args := q.buildArgs(job)

	timeout := q.cfg.TimeoutS
	if timeout <= 0 {
		timeout = 3600
	}
	cmd := exec.Command(exe, args...)
	cmd.Dir = job.OutputDir
	// **系统代理陷阱**：这台机器开着 WinINET 代理，而 pdf2zh 内部的 openai
	// SDK 用 httpx，只读 NO_PROXY 环境变量、**不读注册表的 ProxyOverride**。
	// 不设这一项，发往 127.0.0.1 的每一个请求都会进那个代理并回 502 ——
	// 实测：51 个连续失败任务，两端都看不到原因（网关根本没收到请求）。
	cmd.Env = withNoProxy(os.Environ())
	hideWindow(cmd)

	// **管道死锁陷阱**：pdf2zh_next.exe 是 launcher，它会再 spawn 一个 python
	// 子进程。把 Stdout 设成 io.Writer 时 Go 会建 os.Pipe 并起一个拷贝 goroutine，
	// 而 cmd.Wait() 要等那个 goroutine 结束 —— 也就是等**所有**持有写端的人关闭，
	// 包括那个孙进程。
	//
	// 实测代价：kill launcher 之后孙进程仍活着，写端没关，Wait 永不返回，worker
	// goroutine 被永久阻塞，队列就此停摆（后续 job 永远 queued；没有 panic、没有
	// 错误、网关本身健康）。这是最难查的失败形态：一切都看起来在。
	//
	// WaitDelay 是正解：进程退出后再给 I/O 一点收尾时间，超时强制断开让 Wait
	// 返回。比「杀进程树」安全——后者在 Windows 上会连带杀掉共享控制台的进程
	// （本项目早先踩过）。
	cmd.WaitDelay = 5 * time.Second

	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	if err := cmd.Start(); err != nil {
		return 0, "", nil, fmt.Errorf("spawn %s: %w", filepath.Base(exe), err)
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	// 记下当前进程，好让 Cancel 真的能停掉它。
	//
	// 只把 job 标成 cancelled 是不够的：一个跑了一小时的 job 会让整个串行队列
	// 陪它等一小时，而用户以为已经取消了。
	q.mu.Lock()
	if q.running == nil {
		q.running = make(map[string]*exec.Cmd)
	}
	q.running[job.ID] = cmd
	q.mu.Unlock()
	defer func() {
		q.mu.Lock()
		delete(q.running, job.ID)
		q.mu.Unlock()
	}()

	var waitErr error
	select {
	case waitErr = <-done:
	case <-time.After(time.Duration(timeout) * time.Second):
		// 超时要显式落成失败：让它继续跑会把队列永远占住，而用户只看到
		// 「running」不动。WaitDelay 保证 Kill 之后 Wait 不会因为孙进程
		// 占着管道而再卡住。
		_ = cmd.Process.Kill()
		<-done
		out := buf.String()
		q.mu.Lock()
		if job.State != StateCancelled {
			job.OutputTail = tailOf(out, 4000)
		}
		q.mu.Unlock()
		return 0, out, nil, fmt.Errorf("timed out after %ds", timeout)
	}

	out := buf.String()
	q.mu.Lock()
	job.OutputTail = tailOf(out, 4000)
	q.mu.Unlock()

	code := 0
	if waitErr != nil {
		if ee, ok := waitErr.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else {
			return 0, out, nil, waitErr
		}
	}
	return code, out, findDualPDFs(job.OutputDir, job.Source), nil
}

// Enabled 翻译是否启用。界面据此区分「引擎坏了」和「这个功能我关了」——
// 两件事对用户的含义完全不同，混成一句话会让人去修一个没坏的东西。
func (q *Queue) Enabled() bool { return q.cfg.Enabled }

// Pdf2zhStatus pdf2zh 可执行文件的体检结果，供面板提前展示。
//
// 为什么要在界面上说：新装用户最容易卡住的一步就是 pdf2zh 没装或路径写错。
// 现状是他点「提交翻译」，等几秒，看到一条失败 —— 而真正的信息（"这个文件
// 不存在"）藏在任务详情里。提前一句话就能省掉这一整轮。
//
// 这是**可选**能力：解析失败不影响队列工作，只影响能不能真的翻译。
type Pdf2zhStatus struct {
	Path  string `json:"path,omitempty"`
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
	// Source 说明这个路径是怎么来的：config（显式配置）还是 path（PATH 查找到）。
	Source string `json:"source,omitempty"`
}

func (q *Queue) Pdf2zhStatus() Pdf2zhStatus {
	if q.cfg.PDF2zhExe != "" {
		exe, err := q.resolveExe()
		if err != nil {
			return Pdf2zhStatus{Path: q.cfg.PDF2zhExe, Error: err.Error(), Source: "config"}
		}
		return Pdf2zhStatus{Path: exe, OK: true, Source: "config"}
	}
	exe, err := q.resolveExe()
	if err != nil {
		return Pdf2zhStatus{Error: err.Error(), Source: "path"}
	}
	return Pdf2zhStatus{Path: exe, OK: true, Source: "path"}
}

func (q *Queue) resolveExe() (string, error) {
	if q.cfg.PDF2zhExe != "" {
		if _, err := os.Stat(q.cfg.PDF2zhExe); err != nil {
			return "", fmt.Errorf("pdf2zh_exe not found: %s", q.cfg.PDF2zhExe)
		}
		return q.cfg.PDF2zhExe, nil
	}
	exe, err := exec.LookPath("pdf2zh_next")
	if err != nil {
		return "", fmt.Errorf("pdf2zh_next not on PATH; set translate.pdf2zh_exe")
	}
	return exe, nil
}

// buildArgs 组 pdf2zh 命令行。
//
// 每一个参数都在下面单独说明了它为什么必须是这样。抄错一个就会得到一份
// 「看起来成功」的漏译 PDF。
func (q *Queue) buildArgs(job *Job) []string {
	langIn, langOut := q.cfg.LangIn, q.cfg.LangOut
	if langIn == "" {
		langIn = "en"
	}
	if langOut == "" {
		langOut = "zh-CN"
	}
	// 模型名：网关这一侧用 combo 概念时是「自动选车道」，这里统一用 auto。
	model := job.Model
	if model == "" || model == "combo:translate-free" {
		// 回退顺序：调用方指定的 > 配置的默认模型 > auto。
		// 配置那次回退是本条的目的：插件不带模型，所以它决定了插件实际用什么。
		model = q.cfg.DefaultModel
		if model == "" {
			model = "auto"
		}
	}

	args := []string{
		// 注意是 --openaicompatible（一个词），不是 --openai。
		"--openaicompatible",
		"--openai-compatible-model", model,
		"--openai-compatible-base-url", q.baseURL,
		"--openai-compatible-timeout", ClientTimeoutS,
		"--openai-compatible-api-key", nonEmpty(q.apiKey, PlaceholderKey),
		"--lang-in", langIn,
		"--lang-out", langOut,
		// 只要双语，不要单语（单语会把原文覆盖掉，用户就无从对照）。
		"--no-mono",
		"--qps", job.QPS,
		"--term-qps", job.QPS,
		// 去掉水印：这不是给出版社导出的成品。
		"--watermark-output-mode", "no_watermark",
		"--output", job.OutputDir,
	}
	if !q.cfg.AutoGlossary {
		// 术语自动抽取是**第二遍全文档 LLM 调用**，请求量翻倍。默认关。
		args = append(args, "--no-auto-extract-glossary")
	}
	// 术语表：逗号分隔的 CSV 路径列表（见 glossary.go）。
	// 路径含逗号的情况在 Submit 阶段已被拒，这里不会失败。
	if arg, err := glossaryArg(q.cfg.Glossaries); err == nil && arg != "" {
		args = append(args, "--glossaries", arg)
	}
	if q.cfg.IgnoreCache {
		// 见 Config.IgnoreCache：不开这个，修好一个模型再跑拿到的是旧译文。
		args = append(args, "--ignore-cache")
	}
	args = append(args, job.Source)
	return args
}

// findDualPDFs 找本次产出的双语文件。
//
// 按**源文件名 stem** 匹配，而不是 glob 所有 *.dual.pdf：后者会把磁盘上
// 上一次运行的产物当成本次结果报出去。
func findDualPDFs(dir, source string) []string {
	stem := strings.TrimSuffix(filepath.Base(source), filepath.Ext(source))
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasPrefix(name, stem) {
			continue
		}
		low := strings.ToLower(name)
		if strings.Contains(low, "dual") && strings.HasSuffix(low, ".pdf") {
			out = append(out, filepath.Join(dir, name))
		}
	}
	sort.Strings(out)
	return out
}

// parseTokenTotal pdf2zh 自报的 token 总量。-1 = 没说。
func parseTokenTotal(output string) int64 {
	if output == "" {
		return -1
	}
	m := serialQueueBodyRe.FindAllStringSubmatch(output, -1)
	if len(m) == 0 {
		return -1
	}
	// 取最后一个：输出是追加的，前面的已经过期。
	v, err := strconv.ParseInt(m[len(m)-1][1], 10, 64)
	if err != nil {
		return -1
	}
	return v
}

// CJKCount 数中文汉字数。用于**验证产出**而不是显示进度。
//
// 这是本项目唯一一个不依赖任何一方自述的质量判据：一份十页论文的完整译文
// 应在 8000–15000 之间，低于 2000 就是部分翻译。实测把「0.7% 中文」和
// 「11.8% 中文」区分开的就是这个数。
func CJKCount(text string) int {
	n := 0
	for _, r := range text {
		if unicode.Is(unicode.Han, r) {
			n++
		}
	}
	return n
}

func tailOf(s string, limit int) string {
	if limit <= 0 {
		limit = 4000
	}
	s = strings.TrimSpace(s)
	if len(s) <= limit {
		return s
	}
	return s[len(s)-limit:]
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) == 0 {
		return ""
	}
	last := strings.TrimSpace(lines[len(lines)-1])
	if len(last) > 200 {
		last = last[:200]
	}
	return last
}

func nonEmpty(v, fallback string) string {
	if strings.TrimSpace(v) == "" {
		return fallback
	}
	return v
}

func nowS() float64 { return float64(time.Now().UnixNano()) / 1e9 }

var idMu sync.Mutex
var idSeq uint64

func newID() string {
	idMu.Lock()
	idSeq++
	seq := idSeq
	idMu.Unlock()
	return fmt.Sprintf("%x-%x", time.Now().UnixNano()&0xffffffffff, seq)
}

// MarshalJob 给面板用：job 的可 JSON 化视图。
func MarshalJob(j *Job) map[string]any {
	// nil 防护：这个函数是**界面和插件唯一能看到任务的地方**，而它被 HTTP 处理
	// 路径直接调用。一个 nil 会变成 handler 里的 panic —— net/http 会兜住不崩
	// 进程，但客户端拿到的是断开的连接，而不是可读的错误。
	//
	// 正常路径不会传 nil；真传了说明上游逻辑错了，此时返回 id 为空的形状，
	// 让调用方走到自己的「没有 job id」分支并报出原因（插件正是这么做的）。
	if j == nil {
		return map[string]any{"id": "", "state": "", "error": "internal: nil job"}
	}
	v := map[string]any{
		"id":               j.ID,
		"source":           j.Source,
		"output_dir":       j.OutputDir,
		"model":            j.Model,
		"state":            j.State,
		"liveness":         j.Liveness(),
		"elapsed_s":        round1(j.ElapsedS()),
		"created_at":       j.CreatedAt,
		"started_at":       j.StartedAt,
		"finished_at":      j.FinishedAt,
		"dual_pdfs":        j.DualPDFs,
		"error":            j.Error,
		// 失败类别。界面用它决定给什么建议；error 里已附了对应的一句话，
		// 但类别本身要单独送出去 —— 没有它，面板只能靠 match 中文文案来分类。
		"fail_kind": j.FailKind,

		"produced_nothing": j.ProducedNothing,
		"partial":          j.Partial,
		"warning":          j.Warning,
		"output_tail":      j.OutputTail,
		"token_total":      j.TokenTotal,
	}
	if b, err := json.Marshal(v); err == nil {
		var back map[string]any
		if json.Unmarshal(b, &back) == nil {
			return back
		}
	}
	return v
}

func round1(f float64) float64 {
	return float64(int64(f*10+0.5)) / 10
}

// ReadCJK 读一个 PDF 的文本并数汉字。需要 pdf 解析，故仅在有外部工具时用。
// 这里不实现 PDF 解析——那是 pdf2zh/BabelDOC 的职责，重复实现只会引入
// 第二个「读出来的文本不一样」的源。
var _ = io.Discard
