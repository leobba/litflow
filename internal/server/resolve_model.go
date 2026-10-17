package server

import "strings"

// resolveModel 解析模型名协议（PLAN D6）：
//
//	分布式前缀： "[realm:]model"
//
// 取第一个 ":"，前段恰为 "cn"/"global" 才剥离；否则视为裸名，realm=cn、bare=原串。
// 大小写敏感（前缀必须是精确的小写枚举）。bare 即出站/选号/账本使用的裸模型名。
//
// 导出为 ResolveModel（cmd/server/main.go 粘性闭包需要），包内简写 resolveModel。
func resolveModel(model string) (realm, bare string) {
	idx := strings.IndexByte(model, ':')
	if idx < 0 {
		return "cn", model
	}
	prefix := model[:idx]
	if prefix != "cn" && prefix != "global" {
		return "cn", model
	}
	return prefix, model[idx+1:]
}

// ResolveModel 是 resolveModel 的导出面（跨包调用）。
func ResolveModel(model string) (realm, bare string) { return resolveModel(model) }

// modelChainFor 返回某个请求模型对应的跨模型轮换链。
//
// 只在两种情况下给链，其余一律返回空：
//
//   - 请求的就是 "auto"。auto 的语义是「你自己挑一个」，所以替它挑链上的
//     第一个是兑现承诺；而对一个明确点名的模型换模型，是在用户不知情时换了
//     他的选择，那正是本项目一直在防的事。
//   - 请求的恰好是链上某个成员。这时把链截成从该成员开始的后缀，用户点名的
//     那个仍是第一个被尝试的。
//
// 链为空（未配置）时恒返回空 → 行为与没有这个功能时逐字一致。
func (h *Handler) modelChainFor(model string) []string {
	chain := h.cfg.TranslateChain
	if len(chain) == 0 {
		return nil
	}
	// **返回副本**：h.cfg 是所有请求共享的，任何一个调用方就地改写（比如取
	// 后缀后调整顺序）都会静默污染后续全部请求的链。这个缺陷是被
	// TestModelChainForIsStable 抓到的——读代码时它完全看不出来。
	clone := func(s []string) []string {
		out := make([]string, len(s))
		copy(out, s)
		return out
	}
	if model == "auto" || model == "" {
		return clone(chain)
	}
	for i, m := range chain {
		if m == model {
			return clone(chain[i:])
		}
	}
	return nil
}

// nextChainModel 判断「模型被级阻塞时该不该换、换成谁」。
//
// 抽成纯函数的目的不是为了复用，是为了**可证明**：这段逻辑原本内联在一个九百行
// 的 handler 里，只能靠「真撞出一次 6004」来验证，而那取决于账号当前健不健康——
// 一个正常工作的账号永远不会触发它，于是这条最重要的保护等于没测过。
//
// 三条规则，每条都有代价：
//   - 只有 blocked 才换。若只是「号全挂了」（账号级冷却/在途占满），换模型同样
//     选不到号，还白烧换号预算。
//   - 只往前换，不回头。回头就是死循环。
//   - 换的次数以链长为界，防止链配置异常时无限轮转。
func nextChainModel(chain []string, index, switches int,
	blocked bool) (string, int, bool) {
	if !blocked || len(chain) == 0 {
		return "", index, false
	}
	if switches >= len(chain) {
		return "", index, false
	}
	if index+1 >= len(chain) {
		return "", index, false
	}
	return chain[index+1], index + 1, true
}
