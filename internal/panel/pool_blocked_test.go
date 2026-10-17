package panel

// 全池模型封禁的接线测试。
//
// 守的是**接线**，不是判据：判据在 pool.ModelBlocked 里（modelblocked_test.go 已覆盖）。
// 这里要防的是这条链路断掉 —— 条目 id 带 realm 前缀（cn:hy3），而封禁表按裸名
// （hy3）索引。少剥一次前缀就永远匹配不上，表现为这个标记**永不出现且没有任何报错**，
// 而它恰恰是界面上唯一能说「这个模型现在真的用不了」的地方。
//
// 实测背景：账号池里显示 cn:hy3「预计 15时23分 后重试」，而翻译照跑不误 ——
// 因为那只是**一个账号**的 11102 避让，另一个账号有这个模型。用户据此以为
// 面板在乱报。两者混在一起的根源就是分不清「账号级避让」与「全池封禁」。

import (
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
)

func TestAnnotatePoolBlockedStripsRealmPrefix(t *testing.T) {
	out := []map[string]any{
		{"id": "cn:hy3"},
		{"id": "global:hy3"},
		{"id": "cn:glm-5.3"},
	}
	var asked []string
	blocked := func(model string) pool.ModelBlockStatus {
		asked = append(asked, model)
		if model == "hy3" {
			return pool.ModelBlockStatus{
				Blocked: true, Count: 2, Reason: "11102 model not available",
				Until: time.Date(2026, 10, 18, 12, 38, 0, 0, time.UTC),
			}
		}
		return pool.ModelBlockStatus{}
	}

	annotatePoolBlocked(out, blocked)

	// 查表必须用裸名：带前缀就永远查不中。
	for _, m := range asked {
		if len(m) > 4 && m[:4] == "cn:g" {
			t.Errorf("查表用了带前缀的名字 %q —— 封禁表按裸名索引，这样永远匹配不上", m)
		}
	}

	for _, e := range out {
		_, got := e["pool_blocked"]
		want := e["id"] != "cn:glm-5.3"
		if got != want {
			t.Errorf("id=%v pool_blocked=%v，期望 %v", e["id"], got, want)
		}
	}

	// 命中时三个附注字段都要带上，否则界面只能显示「不可用」而没有原因和时间。
	hit := out[0]
	if hit["pool_block_count"] != 2 {
		t.Errorf("pool_block_count = %v，期望 2", hit["pool_block_count"])
	}
	if hit["pool_block_reason"] != "11102 model not available" {
		t.Errorf("pool_block_reason = %v", hit["pool_block_reason"])
	}
	if _, ok := hit["pool_block_until"]; !ok {
		t.Error("有 Until 时必须带上，界面要显示最早解封时间")
	}
}

// TestAnnotatePoolBlockedSkipsUnblocked 未被全池封禁的条目不能带任何标记。
// 全池是「每个非禁用账号都冷却」——只要还有一个能用的账号，模型就能用。
func TestAnnotatePoolBlockedSkipsUnblocked(t *testing.T) {
	out := []map[string]any{{"id": "cn:ok"}}
	annotatePoolBlocked(out, func(string) pool.ModelBlockStatus {
		return pool.ModelBlockStatus{}
	})
	for k := range out[0] {
		if k != "id" {
			t.Errorf("未封禁的条目不该多出字段 %q", k)
		}
	}
}

// TestAnnotatePoolBlockedTolerantOfBadInput 畸形条目不能让整个列表炸掉。
func TestAnnotatePoolBlockedTolerantOfBadInput(t *testing.T) {
	out := []map[string]any{
		{},                      // 没有 id
		{"id": 12345},           // id 不是字符串
		{"id": ""},              // 空 id
		{"id": ":"},             // 只有分隔符
		{"id": "cn:"},           // 前缀后为空
	}
	var asked []string
	annotatePoolBlocked(out, func(m string) pool.ModelBlockStatus {
		asked = append(asked, m)
		return pool.ModelBlockStatus{}
	})
	// 空裸名不该拿去查表。
	for _, m := range asked {
		if m == "" {
			t.Error("空裸名不该查表 —— 那会命中「模型名为空」的全局条目，语义不明")
		}
	}
}

// TestAnnotatePoolBlockedNilFn 没有池子（测试/裸用路径）时安静跳过。
func TestAnnotatePoolBlockedNilFn(t *testing.T) {
	out := []map[string]any{{"id": "cn:hy3"}}
	annotatePoolBlocked(out, nil) // 不该 panic
	if _, ok := out[0]["pool_blocked"]; ok {
		t.Error("没有池子时不该标封禁")
	}
}
