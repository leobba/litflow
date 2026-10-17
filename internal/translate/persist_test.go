package translate

// 队列落盘的测试。
//
// 这一组断言的重点不是「写得进去」，而是**恢复时的状态语义**：一个未开始的
// 任务和一个跑到一半被打断的任务，事实完全不同，恢复后必须长得不一样。
// 把 running 一律改回 queued 会让人以为它还在跑，而 pdf2zh 已经死了。

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func newTestQueue(t *testing.T, path string) *Queue {
	t.Helper()
	q := NewQueue(DefaultConfig(), "http://127.0.0.1:7863/v1", "k")
	q.SetPersistPath(path)
	return q
}

// seed 直接往内存队列里塞任务（同包，可访问私有字段）。
func seed(q *Queue, jobs ...*Job) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, j := range jobs {
		q.jobs[j.ID] = j
		q.order = append(q.order, j.ID)
	}
}

func TestPersistRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "jobs.json")

	q1 := newTestQueue(t, path)
	seed(q1,
		&Job{ID: "a", Source: "/p/a.pdf", OutputDir: dir, State: StateDone,
			CreatedAt: nowS(), FinishedAt: nowS(), TokenTotal: 1234,
			DualPDFs: []string{"/p/a.dual.pdf"}},
		&Job{ID: "b", Source: "/p/b.pdf", OutputDir: dir, State: StateFailed,
			CreatedAt: nowS(), FinishedAt: nowS(), Error: "boom",
			ProducedNothing: true},
	)
	q1.save()

	if _, err := os.Stat(path); err != nil {
		t.Fatalf("落盘文件不存在: %v", err)
	}

	q2 := newTestQueue(t, path)
	got := q2.List(10)
	if len(got) != 2 {
		t.Fatalf("恢复出 %d 条，期望 2 条", len(got))
	}
	// List 是新的在前
	byID := map[string]*Job{}
	for _, j := range got {
		byID[j.ID] = j
	}
	if j := byID["a"]; j == nil || j.State != StateDone || j.TokenTotal != 1234 {
		t.Errorf("a 恢复错了: %+v", byID["a"])
	}
	if j := byID["b"]; j == nil || j.State != StateFailed || !j.ProducedNothing {
		t.Errorf("b 恢复错了（produced_nothing 必须一起落盘，否则历史看起来都是好的）: %+v", byID["b"])
	}
}

// TestRestoreRunningBecomesFailed 是本文件最重要的一条。
func TestRestoreRunningBecomesFailed(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "jobs.json")

	q1 := newTestQueue(t, path)
	seed(q1, &Job{ID: "r", Source: "/p/r.pdf", OutputDir: dir,
		State: StateRunning, CreatedAt: nowS(), StartedAt: nowS()})
	q1.save()

	q2 := newTestQueue(t, path)
	j := q2.Get("r")
	if j == nil {
		t.Fatal("恢复后找不到任务 r")
	}
	if j.State == StateRunning {
		t.Fatal("running 被原样恢复成了 running —— 子进程已经随网关死了，" +
			"这个状态在骗人：界面会显示它在跑，实际没有任何东西在写产出")
	}
	if j.State != StateFailed {
		t.Fatalf("running 应恢复为 failed，实际 %s", j.State)
	}
	if j.Error == "" {
		t.Fatal("失败原因必须写清楚，否则用户只看到「失败」两个字")
	}
	if j.FinishedAt == 0 {
		t.Error("失败任务应当有结束时间")
	}
}

// TestRestoreQueuedIsRequeued 未开始的任务重排是无损的。
func TestRestoreQueuedIsRequeued(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "jobs.json")

	q1 := newTestQueue(t, path)
	seed(q1, &Job{ID: "q", Source: "/p/q.pdf", OutputDir: dir,
		State: StateQueued, CreatedAt: nowS()})
	q1.save()

	q2 := newTestQueue(t, path)
	j := q2.Get("q")
	if j == nil || j.State != StateQueued {
		t.Fatalf("queued 应保持 queued，实际 %+v", j)
	}
	// 必须真的进了 pending，否则「保持 queued」只是显示上的，永远不会被跑。
	select {
	case id := <-q2.pending:
		if id != "q" {
			t.Errorf("重新入队的是 %q，期望 q", id)
		}
	default:
		t.Error("queued 任务没有重新进入 pending 通道 —— 它会永远停在排队状态")
	}
}

// TestCorruptFileIsQuarantinedNotDeleted 损坏文件不能静默覆盖。
func TestCorruptFileIsQuarantinedNotDeleted(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "jobs.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}

	q := newTestQueue(t, path) // 不该 panic
	if n := len(q.List(10)); n != 0 {
		t.Fatalf("损坏文件应恢复为空队列，实际 %d 条", n)
	}

	entries, _ := os.ReadDir(dir)
	var quarantined string
	for _, e := range entries {
		if len(e.Name()) > len("jobs.json.corrupt-") && filepath.Ext(e.Name()) != ".json" {
			quarantined = e.Name()
		}
	}
	if quarantined == "" {
		t.Fatal("损坏的历史文件必须改名留档 —— 直接覆盖等于把用户的记录删掉，" +
			"而它可能正是排查问题的唯一线索")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("改名之后原路径不该还存在")
	}
}

// TestSaveIsAtomicNoStrayTmp 写完不留 .tmp。
func TestSaveIsAtomicNoStrayTmp(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "jobs.json")
	q := newTestQueue(t, path)
	seed(q, &Job{ID: "x", Source: "/p/x.pdf", OutputDir: dir, State: StateDone, CreatedAt: nowS()})
	q.save()
	q.save()

	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Error("写完仍留下 .tmp —— rename 之后应当消失")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var f persistedFile
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatalf("落盘内容不是合法 JSON: %v", err)
	}
	if f.Version != persistVersion {
		t.Errorf("版本号 = %d，期望 %d", f.Version, persistVersion)
	}
}

// TestPersistOffByDefault 没配置路径时什么都不做（裸用/测试场景）。
func TestPersistOffByDefault(t *testing.T) {
	q := NewQueue(DefaultConfig(), "http://x/v1", "k")
	q.save() // 不该 panic，也不该写文件
	if q.persistPath != "" {
		t.Fatalf("默认应当不落盘，实际 %q", q.persistPath)
	}
}

// TestOutputTailIsCapped 输出尾部有限长，否则 200 条 × 4KB 会把文件撑到 800KB。
func TestOutputTailIsCapped(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "jobs.json")
	q1 := newTestQueue(t, path)
	long := ""
	for i := 0; i < 5000; i++ {
		long += "x"
	}
	seed(q1, &Job{ID: "t", Source: "/p/t.pdf", OutputDir: dir,
		State: StateFailed, CreatedAt: nowS(), OutputTail: long})
	q1.save()

	raw, _ := os.ReadFile(path)
	var f persistedFile
	_ = json.Unmarshal(raw, &f)
	if len(f.Jobs) != 1 {
		t.Fatalf("期望 1 条，实际 %d", len(f.Jobs))
	}
	if n := len(f.Jobs[0].OutputTail); n > tailLimit {
		t.Errorf("OutputTail 落了 %d 字节，上限 %d", n, tailLimit)
	}
}
