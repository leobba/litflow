package server

import (
	"reflect"
	"testing"
)

// TestModelChainFor 覆盖跨模型轮换链的选取规则。
//
// 这条链存在的理由是一个实测故障：某个模型进入 6004 软限流后，池子里每个号对它
// 都不可用，选号返回 nil，网关回 503；而 pdf2zh 收到 503 只会用同一个模型重试，
// 于是整篇论文卡住（实测单账号 soft_rate 窗口内 95 请求 47 个 503，两页论文从
// 104 秒变 200 秒以上）。
//
// 这里的断言集中在「什么时候允许换模型」——因为换错比不换更糟：用户明确点名
// 一个模型时替他换掉，就是在本项目一直要防的那种「用户不知情」。
func TestModelChainFor(t *testing.T) {
	chain := []string{"cn:glm-5.3-flash", "cn:hy3", "cn:step-5-preview"}

	cases := []struct {
		name  string
		chain []string
		model string
		want  []string
	}{
		{
			name:  "未配置链时恒为空（零行为变化）",
			chain: nil,
			model: "auto",
			want:  nil,
		},
		{
			name:  "auto 拿到整条链 —— auto 的语义就是「你来挑」",
			chain: chain,
			model: "auto",
			want:  chain,
		},
		{
			name:  "空模型名等同 auto",
			chain: chain,
			model: "",
			want:  chain,
		},
		{
			name:  "点名链上一个成员时，从它开始的后缀（它仍第一个被试）",
			chain: chain,
			model: "cn:hy3",
			want:  []string{"cn:hy3", "cn:step-5-preview"},
		},
		{
			name:  "点名链上第一个成员，拿到整条链",
			chain: chain,
			model: "cn:glm-5.3-flash",
			want:  chain,
		},
		{
			name:  "点名链上最后一个成员，只剩它自己",
			chain: chain,
			model: "cn:step-5-preview",
			want:  []string{"cn:step-5-preview"},
		},
		{
			name:  "点名一个不在链上的模型 → 不换（用户的选择不被覆写）",
			chain: chain,
			model: "cn:kimi-k3-1",
			want:  nil,
		},
		{
			name:  "空链 + 具体模型 → 不换",
			chain: []string{},
			model: "cn:hy3",
			want:  nil,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := &Handler{cfg: Config{TranslateChain: c.chain}}
			got := h.modelChainFor(c.model)
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("modelChainFor(%q) = %v, want %v", c.model, got, c.want)
			}
		})
	}
}

// TestModelChainForIsStable 链的返回不得与内部切片共享可变状态。
//
// 调用方拿到链后可能就地改写（比如取后缀），如果这里返回的是 cfg 里的底层数组，
// 一次这样的改写就会污染后续所有请求的链——而且是静默的，因为没人会怀疑配置。
func TestModelChainForIsStable(t *testing.T) {
	chain := []string{"a", "b", "c"}
	h := &Handler{cfg: Config{TranslateChain: chain}}

	got := h.modelChainFor("auto")
	if len(got) != 3 {
		t.Fatalf("got %v", got)
	}
	// 调用方改写自己的副本。
	got[0] = "MUTATED"

	again := h.modelChainFor("auto")
	if again[0] != "a" {
		t.Fatalf("chain was mutated through the returned slice: %v", again)
	}
	if chain[0] != "a" {
		t.Fatalf("config chain was mutated: %v", chain)
	}
}

// TestNextChainModel 覆盖「换不换、换成谁」的全部边界。
//
// 这段逻辑此前内联在 handler 里，只能靠真撞一次 6004 来验证；而正常工作的账号
// 永远撞不出来，于是这条保护长期等于没测过。抽出来之后每个分支都能直接断言。
func TestNextChainModel(t *testing.T) {
	chain := []string{"a", "b", "c"}

	cases := []struct {
		name     string
		chain    []string
		index    int
		switches int
		blocked  bool
		wantNext string
		wantIdx  int
		wantOK   bool
	}{
		{"未阻塞不换 —— 号全挂了换模型也没用", chain, 0, 0, false, "", 0, false},
		{"阻塞时前进一格", chain, 0, 0, true, "b", 1, true},
		{"继续前进", chain, 1, 1, true, "c", 2, true},
		{"走到链尾不再换", chain, 2, 2, true, "", 2, false},
		{"空链不换", nil, 0, 0, true, "", 0, false},
		{"只有一个成员的链不换", []string{"a"}, 0, 0, true, "", 0, false},
		{"换满链长后停手（防无限轮转）", chain, 0, 3, true, "", 0, false},
		{"索引越界不 panic", chain, 9, 0, true, "", 9, false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			next, idx, ok := nextChainModel(c.chain, c.index, c.switches, c.blocked)
			if next != c.wantNext || idx != c.wantIdx || ok != c.wantOK {
				t.Fatalf("nextChainModel(%v,%d,%d,%v) = (%q,%d,%v), want (%q,%d,%v)",
					c.chain, c.index, c.switches, c.blocked,
					next, idx, ok, c.wantNext, c.wantIdx, c.wantOK)
			}
		})
	}
}

// TestNextChainModelWalksEveryMemberOnce 完整走一遍链，确认每个成员被访问且
// 只访问一次，最后停手。这是「换模型不会死循环」的直接证明。
func TestNextChainModelWalksEveryMemberOnce(t *testing.T) {
	chain := []string{"m1", "m2", "m3", "m4"}
	index, switches := 0, 0
	visited := []string{chain[0]}
	for i := 0; i < 20; i++ { // 上限远大于链长：若逻辑有循环，这里会超过 4
		next, newIdx, ok := nextChainModel(chain, index, switches, true)
		if !ok {
			break
		}
		index, switches = newIdx, switches+1
		visited = append(visited, next)
	}
	if len(visited) != len(chain) {
		t.Fatalf("visited %v (%d), want every member once (%d)",
			visited, len(visited), len(chain))
	}
	for i, want := range chain {
		if visited[i] != want {
			t.Fatalf("visited %v, want order %v", visited, chain)
		}
	}
}
