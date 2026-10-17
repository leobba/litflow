package translate

// 失败分类的测试。
//
// 这组断言守的是**不说谎**：认不出时必须返回 unknown，而不是靠通用启发式
// 猜一个类别然后给一句自信的错误建议。一个错诊断会让用户去修一个不存在的问题。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestClassifyFailure(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"429 限流", "Error: HTTP 429 Too Many Requests", FailRateLimited},
		{"限流另一种写法", "rate_limit_exceeded: slow down", FailRateLimited},
		{"密钥被拒", "openai error: 401 Unauthorized", FailAuth},
		{"错误的 key", "Incorrect API key provided", FailAuth},
		{"模型不存在", "model not found: cn:nope", FailModel},
		{"积分不足", "insufficient balance", FailQuota},
		{"加密 PDF", "ValueError: PDF is encrypted with a password", FailEncrypted},
		{"扫描件", "no text layer detected; document appears scanned", FailScanned},
		{"网络", "httpx.ConnectError: Connection refused", FailNetwork},
		{"代理", "ProxyError: cannot connect to proxy", FailNetwork},
		{"完全无关的输出", "total nonsense here", FailUnknown},
		{"空输出", "", FailUnknown},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, _ := ClassifyFailure(c.in)
			if got != c.want {
				t.Fatalf("ClassifyFailure(%q) = %q，期望 %q", c.in, got, c.want)
			}
		})
	}
}

// TestClassifyFailurePrefersRateLimitOverNetwork 429 常在输出里伴着 connection
// 重试日志。先判网络会把限流误报成网络问题，用户会去查代理 —— 方向完全错。
func TestClassifyFailurePrefersRateLimitOverNetwork(t *testing.T) {
	out := "retrying after connection error\nHTTP 429 Too Many Requests"
	got, _ := ClassifyFailure(out)
	if got != FailRateLimited {
		t.Fatalf("同时含 connection 与 429 时应判限流，实际 %q", got)
	}
}

// TestClassifyFailureUnknownHasNoHint 认不出时不能给建议。
func TestClassifyFailureUnknownHasNoHint(t *testing.T) {
	kind, hint := ClassifyFailure("完全无法理解的内容")
	if kind != FailUnknown {
		t.Fatalf("应为 unknown，实际 %q", kind)
	}
	if hint != "" {
		t.Errorf("unknown 不该带建议，实际 %q", hint)
	}
}

// TestFailureHintCoversEveryKind 每个类别都要有可执行的建议，否则界面上
// 只剩一个英文标识符。
func TestFailureHintCoversEveryKind(t *testing.T) {
	for _, k := range []string{
		FailRateLimited, FailAuth, FailModel, FailNetwork,
		FailEncrypted, FailScanned, FailQuota,
	} {
		if FailureHint(k) == "" {
			t.Errorf("类别 %q 没有建议文案", k)
		}
	}
	if FailureHint("no-such-kind") != "" {
		t.Error("未知类别应返回空")
	}
}

// ─────────────────────────────────────────────────────────────────────
// 端到端：分类结果必须真的落到 job 上
// ─────────────────────────────────────────────────────────────────────

// TestRunOneRecordsFailKind 非 0 退出时 job 要带上类别与建议。
func TestRunOneRecordsFailKind(t *testing.T) {
	q, job := fakeQueue(t, map[string]string{
		fakePrintEnv: "Error: HTTP 429 Too Many Requests\n",
		fakeExitEnv:  "1",
	})
	q.runOne(job.ID)

	got := q.Get(job.ID)
	if got.FailKind != FailRateLimited {
		t.Fatalf("FailKind = %q，期望 %q", got.FailKind, FailRateLimited)
	}
	if !strings.Contains(got.Error, "限流") {
		t.Errorf("错误文案里应带可执行建议，实际 %q", got.Error)
	}
	if !strings.Contains(got.Error, "exit 1") {
		t.Errorf("仍要保留退出码，实际 %q", got.Error)
	}
}

// TestRunOneUnknownFailureSaysSo 认不出时必须明说，不能编。
func TestRunOneUnknownFailureSaysSo(t *testing.T) {
	q, job := fakeQueue(t, map[string]string{
		fakePrintEnv: "something entirely unparseable happened\n",
		fakeExitEnv:  "2",
	})
	q.runOne(job.ID)

	got := q.Get(job.ID)
	if got.FailKind != FailUnknown {
		t.Fatalf("FailKind = %q，期望 unknown", got.FailKind)
	}
	if !strings.Contains(got.Error, "无法从输出判断原因") {
		t.Errorf("认不出时要明说，实际 %q", got.Error)
	}
	if strings.Contains(got.Error, "限流") || strings.Contains(got.Error, "代理") {
		t.Error("认不出时绝不能给出猜测性的建议")
	}
}

// TestFailKindPersists 类别要跟着历史落盘，否则重启后诊断信息就丢了。
func TestFailKindPersists(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "jobs.json")

	q, job := fakeQueue(t, map[string]string{
		fakePrintEnv: "HTTP 401 Unauthorized\n",
		fakeExitEnv:  "1",
	})
	q.SetPersistPath(path)
	q.runOne(job.ID)
	q.save()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), FailAuth) {
		t.Fatalf("落盘内容里没有失败类别: %s", string(raw))
	}

	q2 := NewQueue(DefaultConfig(), "http://x/v1", "k")
	q2.SetPersistPath(path)
	q2.restore()
	got := q2.Get(job.ID)
	if got == nil {
		t.Fatal("恢复失败")
	}
	if got.FailKind != FailAuth {
		t.Fatalf("恢复后 FailKind = %q，期望 %q", got.FailKind, FailAuth)
	}
}
