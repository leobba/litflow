package translate

// 队列落盘。
//
// 为什么要做：队列原本只活在内存里，一次重启就把所有在飞和已完的任务抹掉。
// 对一个「300 页的书可以跑一小时」的工具这是硬伤 —— 实测代价：我重启网关，
// 一个正在跑的任务消失，而插件那边还在对着一个永远不存在的 id 轮询。
//
// 恢复策略按状态分开，因为它们的**事实**不一样：
//
//	queued   从没开始跑 → 重新排队（没有任何损失，这正是用户按下提交时的期望）
//	running  子进程随网关一起没了 → 标为失败，并说清原因
//	done/failed/skipped/cancelled → 原样保留，作为历史
//
// 把 running 直接改回 queued 是不行的：pdf2zh 进程已经死了，没有任何东西会
// 继续写那个产出目录，重排只会让人以为它还在跑。

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// persistedJob 落盘用的形状。刻意和 Job 分开：Job 里有些字段是运行期观测
// （elapsed 之类靠时间戳算），而落盘只需要事实。
type persistedJob struct {
	ID         string   `json:"id"`
	Source     string   `json:"source"`
	OutputDir  string   `json:"output_dir"`
	Model      string   `json:"model"`
	QPS        string   `json:"qps"`
	State      string   `json:"state"`
	CreatedAt  float64  `json:"created_at"`
	StartedAt  float64  `json:"started_at"`
	FinishedAt float64  `json:"finished_at"`
	DualPDFs   []string `json:"dual_pdfs,omitempty"`
	Error      string   `json:"error,omitempty"`
	// 两种静默失败必须一起落盘，否则恢复后的历史会「看起来都是好的」。
	ProducedNothing bool   `json:"produced_nothing,omitempty"`
	Partial         bool   `json:"partial,omitempty"`
	Warning         string `json:"warning,omitempty"`
	TokenTotal      int64  `json:"token_total"`
	// OutputTail 限制在 1500 字节：200 条 × 4KB = 800KB，而这个文件每天被读一次、
	// 只在出错时才有人看。留最后一段够定位了。
	OutputTail string `json:"output_tail,omitempty"`
}

// persistedFile 是最外层信封。带版本号是为了将来改形状时能识别旧文件，
// 而不是让它解析失败后被当成「没有历史」。
type persistedFile struct {
	Version int            `json:"version"`
	SavedAt float64        `json:"saved_at"`
	Jobs    []persistedJob `json:"jobs"`
}

const persistVersion = 1

// tailLimit 每个任务保留多少字节的输出尾部。
const tailLimit = 1500

// SetPersistPath 设置落盘文件路径并尝试恢复。空路径 = 不落盘。
//
// 必须在 Start() 之前调用：恢复出来的 queued 任务要靠 Start 起 worker 才会跑。
func (q *Queue) SetPersistPath(path string) {
	q.mu.Lock()
	q.persistPath = path
	q.mu.Unlock()
	if path == "" {
		return
	}
	q.restore()
}

// restore 读回历史。文件不存在 = 首次运行，不是错误。
func (q *Queue) restore() {
	q.mu.Lock()
	path := q.persistPath
	q.mu.Unlock()
	if path == "" {
		return
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("[translate] 读不回历史任务（%v），从空队列开始", err)
		}
		return
	}
	var f persistedFile
	if err := json.Unmarshal(raw, &f); err != nil {
		// 解析失败不阻塞启动：一个损坏的历史文件不该让翻译功能整个不可用。
		// 但也绝不静默覆盖——把它改名留档，否则下次保存就把它抹掉了。
		bak := path + ".corrupt-" + time.Now().Format("20060102-150405")
		if e := os.Rename(path, bak); e == nil {
			log.Printf("[translate] 历史任务文件损坏（%v），已另存为 %s，从空队列开始", err, bak)
		} else {
			log.Printf("[translate] 历史任务文件损坏（%v）且无法改名：%v", err, e)
		}
		return
	}

	var requeue []string
	now := nowS()
	q.mu.Lock()
	for _, pj := range f.Jobs {
		j := &Job{
			ID: pj.ID, Source: pj.Source, OutputDir: pj.OutputDir,
			Model: pj.Model, QPS: pj.QPS, State: pj.State,
			CreatedAt: pj.CreatedAt, StartedAt: pj.StartedAt,
			FinishedAt: pj.FinishedAt, DualPDFs: pj.DualPDFs,
			Error: pj.Error, ProducedNothing: pj.ProducedNothing,
			Partial: pj.Partial, Warning: pj.Warning,
			TokenTotal: pj.TokenTotal, OutputTail: pj.OutputTail,
		}
		switch j.State {
		case StateRunning:
			// 子进程随网关一起死了。标为失败并说清原因，而不是假装它还在跑。
			j.State = StateFailed
			j.FinishedAt = now
			j.Error = "网关重启时这个任务正在跑，pdf2zh 子进程随之中断 —— 产出不完整，请重新提交"
			j.OutputTail = tailOf(j.OutputTail, tailLimit)
		case StateQueued:
			// 从没开始跑，重排没有任何损失。
			requeue = append(requeue, j.ID)
		}
		if j.ID == "" {
			continue
		}
		q.jobs[j.ID] = j
		q.order = append(q.order, j.ID)
	}
	q.trimLocked()
	q.mu.Unlock()

	if n := len(q.order); n > 0 || len(requeue) > 0 {
		log.Printf("[translate] 已恢复 %d 条历史任务（其中 %d 条重新排队）", len(f.Jobs), len(requeue))
	}
	// 重新入队要在锁外做：pending 是带缓冲的 channel，满时这里会阻塞。
	for _, id := range requeue {
		select {
		case q.pending <- id:
		default:
			// 缓冲满了说明队列里已经堆了上千个任务，重排这几条没有意义。
			q.mu.Lock()
			if j := q.jobs[id]; j != nil {
				j.State = StateFailed
				j.FinishedAt = now
				j.Error = "恢复时队列已满，无法重新入队"
			}
			q.mu.Unlock()
		}
	}
}

// saveMu 串行化写盘。
//
// 为什么必须要：saveAsync 每次调用都起一个 goroutine，而所有 goroutine 都写
// **同一个** path+".tmp"。两个并发写各自 open(O_TRUNC) 同一个文件，内容会交错：
// 轻则 rename 时报文件不存在，重则把两份快照拼成一份**语义上撕裂但 JSON 合法**的
// 文件，然后原子 rename 把这份坏内容发布出去。下一次启动读它就是错的历史。
//
// 原子写防的是「写到一半崩溃」，防不住「两个自己同时写」—— 这是两件事。
var saveMu sync.Mutex

// save 把当前历史写到磁盘。
//
// 原子写：先写 .tmp 再 rename。直接覆盖的话，写到一半断电就会留下一个截断的
// JSON，而那会让下次启动认不出任何历史 —— 用一次崩溃换掉全部记录。
func (q *Queue) save() {
	q.mu.Lock()
	path := q.persistPath
	if path == "" {
		q.mu.Unlock()
		return
	}
	out := persistedFile{Version: persistVersion, SavedAt: nowS()}
	for _, id := range q.order {
		j := q.jobs[id]
		if j == nil {
			continue
		}
		out.Jobs = append(out.Jobs, persistedJob{
			ID: j.ID, Source: j.Source, OutputDir: j.OutputDir,
			Model: j.Model, QPS: j.QPS, State: j.State,
			CreatedAt: j.CreatedAt, StartedAt: j.StartedAt, FinishedAt: j.FinishedAt,
			DualPDFs: j.DualPDFs, Error: j.Error,
			ProducedNothing: j.ProducedNothing, Partial: j.Partial, Warning: j.Warning,
			TokenTotal: j.TokenTotal,
			OutputTail: tailOf(j.OutputTail, tailLimit),
		})
	}
	q.mu.Unlock()

	// 锁外序列化与写盘：这些 IO 不该挡住 Submit/Cancel。
	saveMu.Lock()
	defer saveMu.Unlock()

	body, err := json.MarshalIndent(out, "", " ")
	if err != nil {
		log.Printf("[translate] 序列化历史失败: %v", err)
		return
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		log.Printf("[translate] 建目录失败 %s: %v", dir, err)
		return
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, body, 0o644); err != nil {
		log.Printf("[translate] 写临时文件失败: %v", err)
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		log.Printf("[translate] 落盘失败: %v", err)
		_ = os.Remove(tmp)
	}
}

// saveAsync 异步落盘：状态转换发生在 worker / HTTP 处理路径上，同步写盘会把
// 一次 200 条的整包重写压到那两条路径上。短暂的延迟完全可以接受 —— 丢的只是
// 最后 120ms 内的状态变化，而恢复逻辑本身对「少一条记录」是安全的。
func (q *Queue) saveAsync() {
	go func() {
		defer func() { _ = recover() }()
		time.Sleep(120 * time.Millisecond)
		q.save()
	}()
}

// OrderSnapshot 供测试与诊断：当前顺序的一份拷贝。
func (q *Queue) OrderSnapshot() []string {
	q.mu.RLock()
	defer q.mu.RUnlock()
	return append([]string(nil), q.order...)
}
