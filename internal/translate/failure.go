package translate

// 失败分类。
//
// 为什么要做：失败时我们现在只把 pdf2zh 的原始输出尾部塞进 job.Error，用户看到
// 的是一堵墙。而真正需要他做的事往往只有一句话 ——「换模型」或者「配 NO_PROXY」。
//
// 设计上刻意保守：**认不出就老实说认不出**，不做通用启发式（比如「取最后一行
// 非堆栈」）。那种做法经常捞到一句无关的 repr，然后以确定的语气给出错误诊断。
// 一个错的诊断比没有诊断更贵 —— 用户会照着它去修一个不存在的问题。
//
// 这些模式来自本机真实遇到过的失败，不是从文档抄的。

import "strings"

// 失败类别。值是稳定的英文标识（进 API、进落盘），显示文案在面板侧映射。
const (
	FailRateLimited = "rate_limited"
	FailAuth        = "auth"
	FailModel       = "model_unavailable"
	FailNetwork     = "network"
	FailEncrypted   = "encrypted"
	FailScanned     = "scanned"
	FailQuota       = "quota"
	FailUnknown     = "unknown"
)

// failurePattern 一个类别 + 命中关键词 + 给用户的一句话。
type failurePattern struct {
	kind string
	hint string
	// keys 用小写子串匹配。刻意选有辨识度的串，避免误判。
	keys []string
}

// 顺序即优先级：越靠前的越具体，越不可能被别的词误命中。
//
// 429 排最前是因为它在本机是最高频的失败，而且它的原始输出里常同时含
// "connection"（重试日志），先判网络会把限流误报成网络问题。
var failurePatterns = []failurePattern{
	{FailRateLimited,
		"账号池被限流了。等几分钟重试，或在面板里换一个模型 —— 轮换链会自动避开刚被限的账号。",
		[]string{"429", "rate limit", "rate_limit", "too many requests", "ratelimit"}},

	{FailAuth,
		"密钥或授权被拒。检查 config.json 的 api_key，以及账号池里的账号是不是过期了。",
		[]string{"401", "403", "incorrect api key", "invalid api key", "unauthorized", "authentication failed"}},

	{FailModel,
		"这个模型不可用。面板「模型与档位」里换一个，或确认它没被上游下架。",
		[]string{"model not found", "no such model", "does not exist", "model_not_found", "404"}},

	{FailQuota,
		"额度/积分不足。面板账号页刷新余额看看。",
		[]string{"insufficient", "not enough", "balance", "quota exceeded", "credit"}},

	{FailEncrypted,
		"PDF 有密码保护。先解开再翻译。",
		[]string{"password", "encrypted", "decrypt"}},

	{FailScanned,
		"这个 PDF 没有文本层（扫描件）。pdf2zh 不会 OCR，需要先做一遍文字识别。",
		[]string{"no text", "scanned", "ocr", "image only", "no extractable"}},

	{FailNetwork,
		"网络或代理问题。本机最常见的原因是系统代理劫持了回环请求 —— 程序已为子进程注入 NO_PROXY，若仍失败请检查 127.0.0.1 是否被代理接管。",
		[]string{"connection", "timed out", "timeout", "proxy", "refused", "unreachable", "ssl", "tls"}},
}

// ClassifyFailure 从输出尾部推断失败类别。
//
// 返回 (unknown, "") 时调用方应当保留原始输出作为主要信息 —— 这是刻意的：
// 不知道就承认不知道。
func ClassifyFailure(output string) (kind string, hint string) {
	if output == "" {
		return FailUnknown, ""
	}
	low := strings.ToLower(output)
	for _, p := range failurePatterns {
		for _, k := range p.keys {
			if strings.Contains(low, k) {
				return p.kind, p.hint
			}
		}
	}
	return FailUnknown, ""
}

// FailureHint 给出某一类失败的建议文案（供界面在只有类别没有输出时使用）。
func FailureHint(kind string) string {
	for _, p := range failurePatterns {
		if p.kind == kind {
			return p.hint
		}
	}
	return ""
}
