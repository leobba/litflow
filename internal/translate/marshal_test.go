package translate

// MarshalJob 的测试。
//
// 这个函数是**界面和插件唯一能看到任务的地方** —— 它漏掉一个字段，那个字段在
// 界面上就等于不存在，而代码完全正确、没有任何报错。加入 fail_kind 时正是靠
// 审查发现的漏项，不是靠测试。

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestMarshalJobCarriesFailKind 失败类别必须出现在响应里。
func TestMarshalJobCarriesFailKind(t *testing.T) {
	j := &Job{
		ID: "x-1", Source: "/p/a.pdf", OutputDir: "/p", Model: "cn:hy3",
		State: StateFailed, CreatedAt: nowS(), FinishedAt: nowS(),
		Error: "pdf2zh exit 1: 429", FailKind: FailRateLimited, TokenTotal: -1,
	}
	m := MarshalJob(j)
	got, ok := m["fail_kind"]
	if !ok {
		t.Fatal("MarshalJob 漏了 fail_kind —— 界面就永远拿不到失败类别")
	}
	if got != FailRateLimited {
		t.Fatalf("fail_kind = %v，期望 %q", got, FailRateLimited)
	}
}

// TestMarshalJobCarriesTheSilentFailureFlags 两个「完成但不对」的标记必须送出去。
// 它们不送，界面就会把一份几乎没翻的 PDF 显示成成功 —— 这正是本项目最怕的那件事。
func TestMarshalJobCarriesTheSilentFailureFlags(t *testing.T) {
	j := &Job{
		ID: "x-2", State: StateDone, CreatedAt: nowS(), FinishedAt: nowS(),
		Partial: true, ProducedNothing: true, Warning: "只有 1.3% 中文",
		TokenTotal: 0,
	}
	m := MarshalJob(j)
	if m["partial"] != true {
		t.Error("partial 未送出")
	}
	if m["produced_nothing"] != true {
		t.Error("produced_nothing 未送出")
	}
	if m["warning"] != "只有 1.3% 中文" {
		t.Errorf("warning 未送出: %v", m["warning"])
	}
	// token_total 是**事实**而不是"未知"：0 表示模型一次都没跑，界面据此
	// 显示「0」而不是「—」。这个区别就是 partial 判定的入口。
	//
	// 注意比较方式：MarshalJob 内部做了一次 JSON 往返（为了把结果归一化成
	// 一定可序列化的形状），所以数字出来是 float64 而不是 int64。这不是 bug，
	// 是那个归一化的必然结果 —— 断言要按实际形状写，否则测的是想象。
	if got, ok := m["token_total"].(float64); !ok || got != 0 {
		t.Errorf("token_total = %v (%T)，期望 0", m["token_total"], m["token_total"])
	}
}

// TestMarshalJobShapesAreJSONSafe 返回值必须是能直接序列化的形状。
// 它内部做了一次 marshal/unmarshal 归一化，如果某个字段变成不可序列化的类型，
// 界面拿到的是空响应而不是报错 —— 又一个静默失败。
func TestMarshalJobShapesAreJSONSafe(t *testing.T) {
	j := &Job{
		ID: "x-3", Source: "/p/带中文的路径.pdf", Model: "cn:hy3",
		State: StateRunning, CreatedAt: nowS(), StartedAt: nowS(),
		DualPDFs: []string{"/p/a.dual.pdf"}, TokenTotal: 12345,
	}
	m := MarshalJob(j)
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("MarshalJob 的结果无法序列化: %v", err)
	}
	s := string(b)
	for _, key := range []string{
		"id", "source", "state", "liveness", "elapsed_s",
		"dual_pdfs", "error", "token_total", "fail_kind",
	} {
		if !strings.Contains(s, `"`+key+`"`) {
			t.Errorf("JSON 里缺少字段 %q", key)
		}
	}
	// 中文路径不能被转义成 \u —— 界面要原样显示。
	if !strings.Contains(s, "带中文的路径") {
		t.Error("中文路径被转义了，界面会显示成一串 \\u")
	}
}

// TestMarshalJobLivenessIsComputed 存活描述是算出来的，不是存的字段。
// 「已用时 3 分 20 秒」这类话必须随时间变化，若返回常量说明接错了。
func TestMarshalJobLivenessIsComputed(t *testing.T) {
	j := &Job{ID: "x-4", State: StateRunning, CreatedAt: nowS(), StartedAt: nowS()}
	if got := MarshalJob(j)["liveness"]; got == nil || got == "" {
		t.Error("运行中的任务应当有存活描述")
	}
	done := &Job{ID: "x-5", State: StateFailed, CreatedAt: nowS(), FinishedAt: nowS()}
	if got := MarshalJob(done)["liveness"]; got == nil {
		t.Error("终态任务也应有存活描述（哪怕是空串）")
	}
}
