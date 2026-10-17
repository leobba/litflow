package translate

// 翻译引擎的测试。
//
// 为什么现在才写：复审发现这个包 19.2% 覆盖，而**承载实测知识的那批函数全是 0%** ——
// buildArgs（全部实测参数）、parseTokenTotal（静默失败检测）、findDualPDFs
// （produced_nothing 的判据）、withNoProxy（头号陷阱）一个都没测。
//
// 更糟的是复审当场抓到一个真 bug：Submit 把 model 硬编码成 "auto"，于是 buildArgs
// 里读 DefaultModel 的分支是死代码 —— 面板选 hy3、接口也返回 cn:hy3，而插件实际
// 打的仍然是 auto。**两处都「正常」，中间的链路断了。**
//
// 所以这组断言的重点是「参数真的到了命令行」和「模型真的被解析」，而不是函数能跑通。

import (
	"encoding/json"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func testQueue(t *testing.T, cfg Config) *Queue {
	t.Helper()
	return NewQueue(cfg, "http://127.0.0.1:7863/v1", "test-key")
}

// ─────────────────────────────────────────────────────────────────────
// buildArgs：每一条参数都是一次实测事故的修复，删掉任何一个都是静默退化
// ─────────────────────────────────────────────────────────────────────

func TestBuildArgsCarriesTheMeasuredFlags(t *testing.T) {
	cfg := DefaultConfig()
	cfg.PDF2zhExe = ""
	cfg.IgnoreCache = false
	cfg.AutoGlossary = false
	q := testQueue(t, cfg)
	job := &Job{Source: `C:\p\a.pdf`, OutputDir: `C:\p`, Model: "cn:hy3", QPS: "2"}

	args := q.buildArgs(job)
	joined := strings.Join(args, " ")

	// 必须存在的参数及其理由
	must := []struct{ flag, why string }{
		{"--openaicompatible", "注意是一个词，不是 --openai；写成 --openai 会认不出"},
		{"--openai-compatible-model cn:hy3", "模型必须真的出现在命令行上，否则白配"},
		{"--openai-compatible-base-url http://127.0.0.1:7863/v1", "必须指回本网关，否则绕过账号池"},
		{"--openai-compatible-api-key", "pdf2zh 没 key 会拒绝启动（是死进程不是更安全的进程）"},
		{"--lang-in en", "源语言"},
		{"--lang-out zh-CN", "目标语言"},
		{"--no-mono", "只要双语；单语会覆盖原文，用户就无从对照"},
		{"--qps 2", "免费池按会话限流，调高不会更快只会更早撞墙"},
		{"--no-auto-extract-glossary", "术语抽取是第二遍全文档调用，请求量翻倍 —— 实测 51 个连续失败任务"},
		{"--watermark-output-mode no_watermark", "这不是给出版社导出的成品"},
	}
	for _, m := range must {
		if !strings.Contains(joined, m.flag) {
			t.Errorf("命令行缺少 %q —— %s\n  实际: %s", m.flag, m.why, joined)
		}
	}
}

// TestIgnoreCacheIsOptIn 默认不开，但开了必须传下去。
// 不开的理由是重跑快；要开的理由是 pdf2zh 的缓存键是「引擎+参数+原文」，
// 修好模型再跑会拿到旧译文，任何 A/B 都变成一次运行和自己比。
func TestIgnoreCacheIsOptIn(t *testing.T) {
	cfg := DefaultConfig()
	job := &Job{Source: `C:\p\a.pdf`, OutputDir: `C:\p`, Model: "m", QPS: "2"}

	q1 := testQueue(t, cfg)
	if strings.Contains(strings.Join(q1.buildArgs(job), " "), "--ignore-cache") {
		t.Error("默认不该传 --ignore-cache")
	}

	cfg.IgnoreCache = true
	q2 := testQueue(t, cfg)
	if !strings.Contains(strings.Join(q2.buildArgs(job), " "), "--ignore-cache") {
		t.Error("配置打开后必须传 --ignore-cache，否则测量拿到的是旧译文")
	}
}

// TestAutoGlossaryOptIn 反过来：默认关，开了就不该有 --no-auto-extract-glossary。
func TestAutoGlossaryOptIn(t *testing.T) {
	cfg := DefaultConfig()
	cfg.AutoGlossary = true
	q := testQueue(t, cfg)
	job := &Job{Source: `C:\p\a.pdf`, OutputDir: `C:\p`, Model: "m", QPS: "2"}
	if strings.Contains(strings.Join(q.buildArgs(job), " "), "--no-auto-extract-glossary") {
		t.Error("AutoGlossary 打开时不该再传 --no-auto-extract-glossary")
	}
}

// TestBuildArgsAppendsSourceLast 源文件必须是最后一个参数。
// pdf2zh 把位置参数当输入文件；放在中间会被后面的 flag 吃掉。
func TestBuildArgsAppendsSourceLast(t *testing.T) {
	q := testQueue(t, DefaultConfig())
	job := &Job{Source: `C:\p\a.pdf`, OutputDir: `C:\p`, Model: "m", QPS: "2"}
	args := q.buildArgs(job)
	if got := args[len(args)-1]; got != job.Source {
		t.Fatalf("最后一个参数是 %q，期望源文件路径 %q", got, job.Source)
	}
}

// ─────────────────────────────────────────────────────────────────────
// 默认模型：复审抓到的真 bug
// ─────────────────────────────────────────────────────────────────────

// TestSubmitResolvesDefaultModel 钉住那个 bug：
// 面板设了 cn:hy3、接口返回 cn:hy3，但插件提交的任务里写的是 auto。
//
// 根因是 Submit 把空 model 硬编码成 "auto"，于是 buildArgs 里读 DefaultModel 的
// 分支永远走不到 —— 死代码。断言放在 Submit 上，因为那才是链路的第一环。
func TestSubmitResolvesDefaultModel(t *testing.T) {
	dir := t.TempDir()
	pdf := filepath.Join(dir, "a.pdf")
	if err := os.WriteFile(pdf, []byte("%PDF-1.4\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := DefaultConfig()
	cfg.DefaultModel = "cn:hy3"
	q := testQueue(t, cfg)

	subRes, err := q.Submit(pdf, dir, "", "", false)
	job := subRes.Job
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if job.Model != "cn:hy3" {
		t.Fatalf("空 model 应解析为 DefaultModel=cn:hy3，实际 %q —— "+
			"这样面板显示的和实际发出的就不一致了", job.Model)
	}

	// 而且要真的出现在命令行上
	if !strings.Contains(strings.Join(q.buildArgs(job), " "), "--openai-compatible-model cn:hy3") {
		t.Error("解析出的模型没有出现在命令行上")
	}
}

// TestSubmitDefaultModelFallsBackToAuto 没配默认模型时退回 auto（并如实记录）。
func TestSubmitDefaultModelFallsBackToAuto(t *testing.T) {
	dir := t.TempDir()
	pdf := filepath.Join(dir, "b.pdf")
	if err := os.WriteFile(pdf, []byte("%PDF-1.4\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := DefaultConfig()
	cfg.DefaultModel = ""
	q := testQueue(t, cfg)
	subRes, err := q.Submit(pdf, dir, "", "", false)
	job := subRes.Job
	if err != nil {
		t.Fatal(err)
	}
	if job.Model != "auto" {
		t.Fatalf("未配默认模型时应退回 auto，实际 %q", job.Model)
	}
}

// TestSubmitExplicitModelWins 调用方点名了就用它的，不许被默认值覆盖。
func TestSubmitExplicitModelWins(t *testing.T) {
	dir := t.TempDir()
	pdf := filepath.Join(dir, "c.pdf")
	os.WriteFile(pdf, []byte("%PDF-1.4\n"), 0o644)
	cfg := DefaultConfig()
	cfg.DefaultModel = "cn:hy3"
	q := testQueue(t, cfg)
	subRes, err := q.Submit(pdf, dir, "cn:step-5-preview", "", false)
	job := subRes.Job
	if err != nil {
		t.Fatal(err)
	}
	if job.Model != "cn:step-5-preview" {
		t.Fatalf("显式指定的模型被覆盖成了 %q", job.Model)
	}
}

// ─────────────────────────────────────────────────────────────────────
// Submit 的校验
// ─────────────────────────────────────────────────────────────────────

func TestSubmitRejectsNonPDF(t *testing.T) {
	dir := t.TempDir()
	txt := filepath.Join(dir, "a.txt")
	os.WriteFile(txt, []byte("x"), 0o644)
	q := testQueue(t, DefaultConfig())
	if _, err := q.Submit(txt, dir, "", "", false); err == nil {
		t.Fatal("非 PDF 必须被拒绝 —— 否则 pdf2zh 会以难懂的方式失败")
	}
}

func TestSubmitRejectsMissingFile(t *testing.T) {
	q := testQueue(t, DefaultConfig())
	if _, err := q.Submit(filepath.Join(t.TempDir(), "nope.pdf"), "", "", "", false); err == nil {
		t.Fatal("不存在的文件必须被拒绝")
	}
}

func TestSubmitRejectsWhenDisabled(t *testing.T) {
	dir := t.TempDir()
	pdf := filepath.Join(dir, "d.pdf")
	os.WriteFile(pdf, []byte("%PDF"), 0o644)
	cfg := DefaultConfig()
	cfg.Enabled = false
	q := testQueue(t, cfg)
	if _, err := q.Submit(pdf, dir, "", "", false); err == nil {
		t.Fatal("翻译关闭时必须报错，而不是静默不排队")
	}
}

// TestSubmitDeduplicatesInFlight 同源在飞时不产生第二个任务。
// Zotero 的通知会对同一次导入触发多次，重复排队等于付费翻两次。
func TestSubmitDeduplicatesInFlight(t *testing.T) {
	dir := t.TempDir()
	pdf := filepath.Join(dir, "e.pdf")
	os.WriteFile(pdf, []byte("%PDF"), 0o644)
	q := testQueue(t, DefaultConfig())

	subRes, err := q.Submit(pdf, dir, "", "", false)
	j1 := subRes.Job
	if err != nil {
		t.Fatal(err)
	}
	// 第二次必须是 deduped，而不是又排一个 —— 这是这个端点唯一能告诉调用方
	// 「本次没有新增工作」的方式。
	if subRes.Outcome != SubmitQueued {
		t.Fatalf("首次提交 outcome = %q，期望 %q", subRes.Outcome, SubmitQueued)
	}
	subRes2, err := q.Submit(pdf, dir, "", "", false)
	j2 := subRes2.Job
	if err != nil {
		t.Fatal(err)
	}
	if subRes2.Outcome != SubmitDeduped {
		t.Fatalf("重复提交 outcome = %q，期望 %q", subRes2.Outcome, SubmitDeduped)
	}
	if j1.ID != j2.ID {
		t.Fatalf("同源在飞时应返回同一个任务，得到 %s 和 %s", j1.ID, j2.ID)
	}
}

// TestSubmitSkipExistingWhenDualPresent 已有双语件时跳过，并说清楚原因。
func TestSubmitSkipExistingWhenDualPresent(t *testing.T) {
	dir := t.TempDir()
	pdf := filepath.Join(dir, "f.pdf")
	os.WriteFile(pdf, []byte("%PDF"), 0o644)
	os.WriteFile(filepath.Join(dir, "f.no_watermark.zh-CN.dual.pdf"), []byte("%PDF"), 0o644)

	q := testQueue(t, DefaultConfig())
	subRes, err := q.Submit(pdf, dir, "", "", true)
	job := subRes.Job
	if err != nil {
		t.Fatal(err)
	}
	if job.State != StateSkipped {
		t.Fatalf("已有双语件时应 skipped，实际 %s", job.State)
	}
	if job.Error == "" {
		t.Error("跳过也要给出原因，否则界面只有一行灰字")
	}
}

// ─────────────────────────────────────────────────────────────────────
// parseTokenTotal：静默失败的判据
// ─────────────────────────────────────────────────────────────────────

func TestParseTokenTotal(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want int64
	}{
		{"没有输出", "", -1},
		{"有输出但没这行", "Translating...\ndone", -1},
		{"Total 0 —— 模型从未跑过", "Total Token Usage: Total 0", 0},
		{"正常数值", "Total Token Usage: Total 32086", 32086},
		{"多处出现时取最后一个", "Total Token Usage: Total 100\n...\nTotal Token Usage: Total 200", 200},
		{"大小写与空格", "total token usage:  total  1234", 1234},
		{"数字非法", "Total Token Usage: Total abc", -1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := parseTokenTotal(c.in); got != c.want {
				t.Fatalf("parseTokenTotal(%q) = %d，期望 %d", c.in, got, c.want)
			}
		})
	}
}

// ─────────────────────────────────────────────────────────────────────
// findDualPDFs：produced_nothing 的判据
// ─────────────────────────────────────────────────────────────────────

func TestFindDualPDFs(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "paper.pdf")
	os.WriteFile(src, []byte("%PDF"), 0o644)
	touch := func(name string) {
		os.WriteFile(filepath.Join(dir, name), []byte("%PDF"), 0o644)
	}

	// 同前缀但不同源的文件不该被当成本次产出
	touch("paper-extra.no_watermark.zh-CN.dual.pdf")
	// 别的论文残留
	touch("other.no_watermark.zh-CN.dual.pdf")
	// 单语件不该算
	touch("paper.no_watermark.zh-CN.mono.pdf")

	got := findDualPDFs(dir, src)
	if len(got) != 1 {
		t.Fatalf("期望只匹配到 paper 的 dual，实际 %v", got)
	}
	if !strings.HasPrefix(filepath.Base(got[0]), "paper") {
		t.Errorf("匹配到了别的论文: %v", got)
	}

	// 完全没有产出时必须返回空 —— 这正是 produced_nothing 的触发条件
	empty := t.TempDir()
	if got := findDualPDFs(empty, filepath.Join(empty, "x.pdf")); len(got) != 0 {
		t.Fatalf("空目录应返回空，实际 %v", got)
	}
}

// ─────────────────────────────────────────────────────────────────────
// withNoProxy：头号陷阱
// ─────────────────────────────────────────────────────────────────────

func TestWithNoProxyAppends(t *testing.T) {
	env := []string{"PATH=/bin", "NO_PROXY=example.com"}
	out := withNoProxy(env)
	joined := strings.Join(out, ";")

	for _, host := range []string{"127.0.0.1", "localhost", "::1"} {
		if !strings.Contains(joined, host) {
			t.Errorf("NO_PROXY 缺少 %s —— 回环请求会被系统代理劫持，"+
				"表现为网关根本收不到请求", host)
		}
	}
	// 原有条目不能被丢掉
	if !strings.Contains(joined, "example.com") {
		t.Error("原有的 NO_PROXY 条目被覆盖了，必须追加")
	}
}

func TestWithNoProxyDedupe(t *testing.T) {
	env := []string{"NO_PROXY=127.0.0.1,localhost", "no_proxy=127.0.0.1"}
	out := withNoProxy(env)
	var found string
	for _, kv := range out {
		if strings.HasPrefix(kv, "NO_PROXY=") {
			found = kv
		}
	}
	if strings.Count(found, "127.0.0.1") != 1 {
		t.Errorf("重复追加了 127.0.0.1: %q", found)
	}
	if strings.Count(found, "localhost") != 1 {
		t.Errorf("重复追加了 localhost: %q", found)
	}
}

// ─────────────────────────────────────────────────────────────────────
// 并发落盘：复审抓到的第二个 bug
// ─────────────────────────────────────────────────────────────────────

// TestConcurrentSaveDoesNotLoseWrites 钉住并发写盘的竞态。
//
// 原来 saveAsync 每次起一个 goroutine，所有 goroutine 写**同一个** path+".tmp"。
// 实测（64 并发、200 条历史）：**43 次 rename 失败**，日志刷出 43 行
// 「落盘失败: ... being used by another process」。
//
// 这里要说清一件事，因为第一版测试把它写错了：后果**不是文件损坏**。
// os.WriteFile 对整块 buffer 是一次 write syscall，内容不会被撕裂；rename 失败
// 只是让这一次保存整体作废。真实伤害是**写入静默丢失** —— 64 次里 43 次没写进去，
// 磁盘上的 jobs.json 会落后于内存，重启后最近的状态就没了，而没有任何报错给用户。
//
// 所以断言的是「一次都不许失败」，而不是「文件能解析」——后者在无互斥时也成立，
// 那样的断言是装饰品（变异测试当场证明了这点）。
func TestConcurrentSaveDoesNotLoseWrites(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "jobs.json")

	cfg := DefaultConfig()
	q := NewQueue(cfg, "http://x/v1", "k")
	q.SetPersistPath(path)

	// 塞满历史（MaxHistory 默认 200），每条带接近上限的输出尾部，
	// 让每次序列化都够大、够慢，把竞态窗口撑开。
	q.mu.Lock()
	for i := 0; i < 200; i++ {
		id := "job-" + strings.Repeat("x", i%7) + "-" + itoaTest(i)
		q.jobs[id] = &Job{
			ID: id, Source: "/p/" + id + ".pdf", OutputDir: dir,
			State: StateDone, CreatedAt: nowS(), OutputTail: strings.Repeat("t", 1400),
		}
		q.order = append(q.order, id)
	}
	q.mu.Unlock()

	// 捕获 log 输出：落盘失败是**只记日志**的，不打到 t.Error，所以必须自己捞。
	var buf syncBuffer
	prev := logOutput()
	logSetOutput(&buf)
	defer logSetOutput(prev)

	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			q.save()
		}()
	}
	wg.Wait()

	logged := buf.String()
	for _, bad := range []string{"落盘失败", "写临时文件失败", "序列化历史失败"} {
		if strings.Contains(logged, bad) {
			n := strings.Count(logged, bad)
			t.Fatalf("并发保存出现 %d 次 %q —— 这些写盘会被静默丢弃，"+
				"磁盘上的历史落后于内存。原样输出：\n%s", n, bad, firstLines(logged, 6))
		}
	}

	// 次要断言：最终文件必须完整可解析（这条在无互斥时也成立，不是主力）。
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读回失败: %v", err)
	}
	var f persistedFile
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatalf("jobs.json 不是合法 JSON: %v", err)
	}
	if len(f.Jobs) != 200 {
		t.Fatalf("恢复出 %d 条，期望 200", len(f.Jobs))
	}
}

// syncBuffer 是并发安全的 bytes.Buffer（log 会被多个 goroutine 写）。
type syncBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func firstLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[:n]
	}
	return "  " + strings.Join(lines, "\n  ")
}


// logSetOutput / logOutput 包一层，把标准库 log 的输出重定向到测试缓冲区。
// 直接调 log.SetOutput 也行，但在这组测试里出现多次，包一下读起来更清楚。
func logSetOutput(w io.Writer) { log.SetOutput(w) }
func logOutput() io.Writer     { return log.Writer() }

// ─────────────────────────────────────────────────────────────────────
// Cancel：当初把整个网关写死的地方
// ─────────────────────────────────────────────────────────────────────

// TestCancelQueuedJob 排队中的任务取消后必须落成 cancelled，且不再被执行。
func TestCancelQueuedJob(t *testing.T) {
	dir := t.TempDir()
	pdf := filepath.Join(dir, "c1.pdf")
	os.WriteFile(pdf, []byte("%PDF"), 0o644)
	q := testQueue(t, DefaultConfig())

	subRes, err := q.Submit(pdf, dir, "", "", false)
	job := subRes.Job
	if err != nil {
		t.Fatal(err)
	}
	if !q.Cancel(job.ID) {
		t.Fatal("排队中的任务应当可以取消")
	}
	if got := q.Get(job.ID); got.State != StateCancelled {
		t.Fatalf("取消后状态应为 cancelled，实际 %s", got.State)
	}
}

// TestCancelTerminalReturnsFalse 已终态的任务不能再取消 —— 返回 false 而不是假装成功。
// 假装成功会让界面把「已完成」显示成「已取消」。
func TestCancelTerminalReturnsFalse(t *testing.T) {
	dir := t.TempDir()
	pdf := filepath.Join(dir, "c2.pdf")
	os.WriteFile(pdf, []byte("%PDF"), 0o644)
	os.WriteFile(filepath.Join(dir, "c2.no_watermark.zh-CN.dual.pdf"), []byte("%PDF"), 0o644)

	q := testQueue(t, DefaultConfig())
	// 已有双语件 → skipped，直接落终态。
	//
	// 断言 Outcome 而不是 State：这是**调用的结果**（什么都没排队），而 State 是
	// 任务的状态。两者在这里恰好一致，但语义不同 —— 有了 Outcome 就不必再靠
	// 「读一下 state 猜刚才发生了什么」。
	subRes, err := q.Submit(pdf, dir, "", "", true)
	job := subRes.Job
	if err != nil {
		t.Fatal(err)
	}
	if subRes.Outcome != SubmitSkipped {
		t.Fatalf("已有双语件应 skipped，outcome = %q", subRes.Outcome)
	}
	if job.State != StateSkipped {
		t.Fatalf("前置条件不成立，状态是 %s", job.State)
	}
	if q.Cancel(job.ID) {
		t.Fatal("已终态的任务不该能取消")
	}
	if got := q.Get(job.ID); got.State != StateSkipped {
		t.Fatalf("状态被改坏了: %s", got.State)
	}
}

func TestCancelUnknownReturnsFalse(t *testing.T) {
	q := testQueue(t, DefaultConfig())
	if q.Cancel("no-such-id") {
		t.Fatal("未知 id 不该返回 true")
	}
}

// TestCancelDoesNotPanicWithLocking 这条是为了守住那个 fatal error。
//
// 原来的 finish() 里既有 `defer q.mu.Unlock()` 又有一句显式 Unlock，
// 第二次解锁是 `fatal error: sync: Unlock of unlocked RWMutex` —— **不是 panic，
// 是直接终止进程**，整个账号池跟着消失。所以这里连续做一串会碰锁的操作，
// 只要进程还活着（测试跑到这里没崩），就说明加解锁是配平的。
// 普通的 -race 抓不到这类错误，它是 runtime 的致命检查。
func TestCancelDoesNotPanicWithLocking(t *testing.T) {
	dir := t.TempDir()
	q := testQueue(t, DefaultConfig())

	for i := 0; i < 20; i++ {
		pdf := filepath.Join(dir, "loop"+itoaTest(i)+".pdf")
		os.WriteFile(pdf, []byte("%PDF"), 0o644)
		subRes, err := q.Submit(pdf, dir, "", "", false)
		job := subRes.Job
		if err != nil {
			t.Fatal(err)
		}
		// 交替取消与读取，把 Cancel / finish / Get / List 的加解锁都走一遍
		q.Cancel(job.ID)
		_ = q.Get(job.ID)
		_ = q.List(10)
	}
	// 能走到这里说明没有 fatal error
}

func itoaTest(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}
