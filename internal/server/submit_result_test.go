package server

// 提交结果的 HTTP 映射测试。
//
// 守的是一句话：**202 的语义是「接下了、还没做完」**，只有真的接下时才该回它。
//
// 而 Submit 成功有三种，旧代码一律回 202 —— 于是 skipped（磁盘上已有译文，什么都没
// 排队）也在说「接下了」。调用方据此会去轮询一个**已经是终态**的任务，而轮询循环
// 里没有任何东西会让它推进。这不是理论问题：跳过是「已有译文」的常见路径。
//
// 同时断言响应体里带 outcome：调用方不必再从 job.state 反推「刚才到底发生了什么」。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/translate"
)

func TestWriteSubmitResultStatusByOutcome(t *testing.T) {
	cases := []struct {
		outcome translate.SubmitOutcome
		want    int
		why     string
	}{
		{translate.SubmitQueued, http.StatusAccepted, "真接下了活，202 正确"},
		{translate.SubmitDeduped, http.StatusOK, "返回的是队列里原来那个，本次没新增工作"},
		{translate.SubmitSkipped, http.StatusOK, "什么都没排队，回 202 是在说「我接下了」"},
	}
	for _, c := range cases {
		rec := httptest.NewRecorder()
		writeSubmitResult(rec, translate.SubmitResult{
			Job:     &translate.Job{ID: "j-1", Source: "/p/a.pdf", State: translate.StateQueued},
			Outcome: c.outcome,
		})
		if rec.Code != c.want {
			t.Errorf("outcome=%q 状态码 %d，期望 %d（%s）", c.outcome, rec.Code, c.want, c.why)
		}
	}
}

// TestWriteSubmitResultCarriesOutcome 响应体必须带上 outcome。
//
// 界面靠它区分「已排上」和「已有译文所以没排」—— 两者都返回同一个 job 形状，
// 光看 job 分不出来。
func TestWriteSubmitResultCarriesOutcome(t *testing.T) {
	for _, outcome := range []translate.SubmitOutcome{
		translate.SubmitQueued, translate.SubmitDeduped, translate.SubmitSkipped,
	} {
		rec := httptest.NewRecorder()
		writeSubmitResult(rec, translate.SubmitResult{
			Job:     &translate.Job{ID: "j-2"},
			Outcome: outcome,
		})
		var body map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("响应不是合法 JSON: %v\n%s", err, rec.Body.String())
		}
		if body["outcome"] != string(outcome) {
			t.Errorf("响应里的 outcome = %v，期望 %q", body["outcome"], outcome)
		}
		// job 的既有字段一个都不能少 —— 插件只读 id / state，删了就断开。
		for _, k := range []string{"id", "state"} {
			if _, ok := body[k]; !ok {
				t.Errorf("响应缺少 job 字段 %q（插件依赖它）", k)
			}
		}
	}
}

// TestWriteSubmitResultNilJobToleratesBadInput job 为空不能 panic。
// 这是防御性的：真出现说明上游逻辑错了，但一个 500 比一个 panic 好排查。
func TestWriteSubmitResultNilJobToleratesBadInput(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("job 为 nil 时 panic 了：%v", r)
		}
	}()
	rec := httptest.NewRecorder()
	writeSubmitResult(rec, translate.SubmitResult{Outcome: translate.SubmitQueued})
	if rec.Code != http.StatusAccepted {
		t.Errorf("状态码 %d，期望 202", rec.Code)
	}
}
