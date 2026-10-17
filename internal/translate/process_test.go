package translate

// 进程与状态机测试：spawn / runOne / finish。
//
// 这三个函数是这个项目立意的落地处 —— `produced_nothing` 和 `partial` 的判断
// 全在这里，而它们的失败形态**恰好是「看起来成功」**。之前它们 0% 覆盖，
// 也就是说项目最核心的三条防线一条都没被测过。
//
// 为什么能不用真的 pdf2zh：把**测试二进制自己**当成假 pdf2zh。TestMain 在
// flag 解析之前检查一个环境变量，命中就以"pdf2zh"的身份跑完退出。于是
// exec.Command(exe, "--openaicompatible", ...) 拿到的是一个行为可控的进程，
// 四种结局（非 0 退出 / 0 退出但无产出 / 有产出但 0 token / 正常完成）都能造。
//
// 这条路径还有个附带价值：它真的走了一遍 exec.Command、环境变量传递、
// 输出捕获、WaitDelay —— 而不是 mock 掉它们。

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 假 pdf2zh 的行为开关（通过环境变量传给它自己）。
const (
	fakeModeEnv   = "LITFLOW_FAKE_PDF2ZH"
	fakeExitEnv   = "FAKE_EXIT"
	fakePrintEnv  = "FAKE_PRINT"
	fakeDualEnv   = "FAKE_DUAL"   // "1" = 在源文件旁边造一份双语件
	fakeSleepEnv  = "FAKE_SLEEP"  // 秒；用来造超时
)

// TestMain 接管进程入口。
//
// 必须在 m.Run() 之前判断：一旦往下走，Go 的 testing 框架会解析命令行，
// 而我们的"命令行"是 pdf2zh 的 flag（--openaicompatible 等），会直接解析失败。
func TestMain(m *testing.M) {
	if os.Getenv(fakeModeEnv) == "1" {
		os.Exit(fakePdf2zh())
	}
	os.Exit(m.Run())
}

// fakePdf2zh 以 pdf2zh 的身份运行：按环境变量决定输出、退出码、是否造产出。
func fakePdf2zh() int {
	if s := os.Getenv(fakePrintEnv); s != "" {
		fmt.Print(s)
	}
	if os.Getenv(fakeDualEnv) == "1" {
		// pdf2zh 把源文件当最后一个位置参数
		args := os.Args[1:]
		if len(args) > 0 {
			src := args[len(args)-1]
			stem := strings.TrimSuffix(filepath.Base(src), filepath.Ext(src))
			out := filepath.Join(filepath.Dir(src),
				stem+".no_watermark.zh-CN.dual.pdf")
			_ = os.WriteFile(out, []byte("%PDF-1.4 fake dual"), 0o644)
		}
	}
	code := 0
	if s := os.Getenv(fakeExitEnv); s != "" {
		fmt.Sscan(s, &code)
	}
	return code
}

// fakeQueue 造一个把 pdf2zh 指向测试二进制的队列。
func fakeQueue(t *testing.T, env map[string]string) (*Queue, *Job) {
	t.Helper()

	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	pdf := filepath.Join(dir, "paper.pdf")
	if err := os.WriteFile(pdf, []byte("%PDF-1.4 src"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := DefaultConfig()
	cfg.PDF2zhExe = exe

	// 假进程从环境变量读行为；用 t.Setenv 保证测试间不串味。
	for k, v := range env {
		t.Setenv(k, v)
	}
	t.Setenv(fakeModeEnv, "1")

	q := NewQueue(cfg, "http://127.0.0.1:7863/v1", "test-key")
	job := &Job{
		ID: "fake-1", Source: pdf, OutputDir: dir,
		Model: "cn:hy3", QPS: "2", State: StateQueued, CreatedAt: nowS(),
		TokenTotal: -1,
	}
	q.mu.Lock()
	q.jobs[job.ID] = job
	q.order = append(q.order, job.ID)
	q.mu.Unlock()
	return q, job
}

// ─────────────────────────────────────────────────────────────────────
// 四种结局
// ─────────────────────────────────────────────────────────────────────

// TestRunOneDone 正常完成：退出 0、有产出、token > 0。
func TestRunOneDone(t *testing.T) {
	q, job := fakeQueue(t, map[string]string{
		fakePrintEnv: "Translating...\nTotal Token Usage: Total 12345\n",
		fakeDualEnv:  "1",
	})
	q.runOne(job.ID)

	got := q.Get(job.ID)
	if got.State != StateDone {
		t.Fatalf("应为 done，实际 %s（error=%q）", got.State, got.Error)
	}
	if got.TokenTotal != 12345 {
		t.Errorf("TokenTotal = %d，期望 12345", got.TokenTotal)
	}
	if got.Partial {
		t.Error("token > 0 时不该标 partial")
	}
	if got.ProducedNothing {
		t.Error("有产出时不该标 produced_nothing")
	}
	if len(got.DualPDFs) != 1 {
		t.Errorf("应记录 1 个双语件，实际 %v", got.DualPDFs)
	}
	if got.FinishedAt == 0 {
		t.Error("终态必须有结束时间")
	}
}

// TestRunOnePartial 有产出但 0 token —— **最容易骗人的一种**。
//
// 磁盘上是真的双语 PDF、退出码 0、每个更早的检查都通过。只有 token 总量说
// 模型一次都没跑。实测：10 页论文回来 1.3% 中文，四页一个字没有。
func TestRunOnePartial(t *testing.T) {
	q, job := fakeQueue(t, map[string]string{
		fakePrintEnv: "Total Token Usage: Total 0\n",
		fakeDualEnv:  "1",
	})
	q.runOne(job.ID)

	got := q.Get(job.ID)
	if got.State != StateDone {
		t.Fatalf("有双语件时状态仍是 done（它不是失败，是「完成但不对」），实际 %s", got.State)
	}
	if !got.Partial {
		t.Fatal("token 总量为 0 时必须标 partial —— 否则界面会把一份几乎没翻的 PDF 显示成成功")
	}
	if got.Warning == "" {
		t.Error("partial 必须带说明，否则用户只看得到一个标记")
	}
}

// TestRunOneProducedNothing 退出 0 但没有双语件 —— 另一个静默失败。
func TestRunOneProducedNothing(t *testing.T) {
	q, job := fakeQueue(t, map[string]string{
		fakePrintEnv: "Total Token Usage: Total 500\n",
		fakeDualEnv:  "0", // 不造产出
	})
	q.runOne(job.ID)

	got := q.Get(job.ID)
	if got.State != StateFailed {
		t.Fatalf("没有产出必须判为 failed（否则界面显示完成而文件不存在），实际 %s", got.State)
	}
	if !got.ProducedNothing {
		t.Error("必须单独标 produced_nothing —— 塞进 error 会让它读起来像崩溃，而它的本质是「看起来成功」")
	}
	if got.Error == "" {
		t.Error("必须给出原因")
	}
}

// TestRunOneNonZeroExit 子进程非 0 退出要把退出码带出来。
func TestRunOneNonZeroExit(t *testing.T) {
	q, job := fakeQueue(t, map[string]string{
		fakePrintEnv: "boom: something failed\n",
		fakeExitEnv:  "3",
	})
	q.runOne(job.ID)

	got := q.Get(job.ID)
	if got.State != StateFailed {
		t.Fatalf("应为 failed，实际 %s", got.State)
	}
	if !strings.Contains(got.Error, "3") {
		t.Errorf("错误里应带退出码 3，实际 %q", got.Error)
	}
	if got.ProducedNothing {
		t.Error("非 0 退出不该被标成 produced_nothing —— 那是另一个类别")
	}
}

// ─────────────────────────────────────────────────────────────────────
// 输出尾部：没有它，失败无法诊断
// ─────────────────────────────────────────────────────────────────────

// TestSpawnRetainsOutputTail 失败时保留输出尾部。
// 只报结果不报原因，唯一剩下的动作是手工重跑碰运气。
func TestSpawnRetainsOutputTail(t *testing.T) {
	q, job := fakeQueue(t, map[string]string{
		fakePrintEnv: "line one\nline two\nfatal: the real reason\n",
		fakeExitEnv:  "1",
	})
	q.runOne(job.ID)

	got := q.Get(job.ID)
	if !strings.Contains(got.OutputTail, "the real reason") {
		t.Fatalf("输出尾部丢了，诊断只能靠猜。实际 %q", got.OutputTail)
	}
}

// ─────────────────────────────────────────────────────────────────────
// 命令组装：走真实 exec，验证参数真的传给了进程
// ─────────────────────────────────────────────────────────────────────

// TestSpawnReceivesSourceAsLastArg 源文件必须是进程收到的最后一个参数。
// 假进程正是靠这个约定找到源文件并造产出的 —— 如果顺序变了，这里会失败。
func TestSpawnReceivesSourceAsLastArg(t *testing.T) {
	q, job := fakeQueue(t, map[string]string{
		fakePrintEnv: "Total Token Usage: Total 9\n",
		fakeDualEnv:  "1",
	})
	// 只要产物被造出来，就证明假进程收到的最后一个参数确实是源文件
	q.runOne(job.ID)

	got := q.Get(job.ID)
	if len(got.DualPDFs) == 0 {
		t.Fatal("假进程没能按「最后一个参数是源文件」造出产出 —— " +
			"要么参数顺序变了，要么源文件没传下去")
	}
	if !strings.Contains(filepath.Base(got.DualPDFs[0]), "paper") {
		t.Errorf("产出的双语件不属于这篇论文: %v", got.DualPDFs)
	}
}

// ─────────────────────────────────────────────────────────────────────
// resolveExe：找不到 pdf2zh 时的报错质量
// ─────────────────────────────────────────────────────────────────────

// TestResolveExeMissingFile 配了路径但文件不存在 → 明确报错，不静默回落 PATH。
func TestResolveExeMissingFile(t *testing.T) {
	cfg := DefaultConfig()
	cfg.PDF2zhExe = filepath.Join(t.TempDir(), "nope.exe")
	q := NewQueue(cfg, "http://x/v1", "k")
	_, err := q.resolveExe()
	if err == nil {
		t.Fatal("配了不存在的路径必须报错 —— 静默回落 PATH 会让人以为配置生效了")
	}
	if !strings.Contains(err.Error(), "pdf2zh_exe") {
		t.Errorf("错误里应指出是哪个配置项，实际 %q", err)
	}
}

// TestResolveExeFromConfig 配了有效路径就用它。
func TestResolveExeFromConfig(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cfg := DefaultConfig()
	cfg.PDF2zhExe = exe
	q := NewQueue(cfg, "http://x/v1", "k")
	got, err := q.resolveExe()
	if err != nil {
		t.Fatal(err)
	}
	if got != exe {
		t.Fatalf("resolveExe = %q，期望 %q", got, exe)
	}
}
