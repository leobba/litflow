package translate

// Zotero 插件「后端只有一条路」与「失败可见」的回归断言。
//
// 为什么放在 translate 包：这个包已经有读插件真源码的基础设施（见
// dualname_agree_test.go 的 indexOf），跨组件一致性检查也在这里。插件本身没有
// Go 代码，不会自己跑测试，所以它最容易被改坏而无人察觉 —— 而它恰恰是用户唯一
// 直接操作的界面。
//
// 这组断言守两个刚刚发生过的判断：
//
//  1. **脚本后端真的删干净了。** 它调用的 batch_translate_live.py 连的是
//     http://127.0.0.1:8787/v1（被本网关取代、早已退役的旧面板），跑不通，且失败
//     只写一行日志。留一条永远跑不通的回退，比一个明确的报错更糟。
//  2. **失败要留下痕迹。** 在此之前翻译失败在 Zotero 里什么都不留：没有报错、
//     没有标签、没有进度窗，用户看到的是「点了没反应」。

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func readPluginJS(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "plugin", "content", "dualsync.js"))
	if err != nil {
		t.Fatalf("读不到插件源码: %v", err)
	}
	return string(raw)
}

// TestPluginHasNoScriptBackend 死掉的脚本后端不能以任何形式回流。
func TestPluginHasNoScriptBackend(t *testing.T) {
	js := readPluginJS(t)

	dead := []struct{ needle, why string }{
		{`kind: "script"`, "脚本后端的返回形态；留着说明还有第二条路"},
		{`kind === "script"`, "脚本后端的分支判断"},
		{`paths.python`, "调 python 执行脚本的入参"},
		{`dualsync.translateScript`, "脚本后端的偏好项"},
	}
	for _, d := range dead {
		if strings.Contains(js, d.needle) {
			t.Errorf("插件里仍残留 %q —— %s", d.needle, d.why)
		}
	}

	// 注意：源码注释里仍会出现那个脚本的名字（用来说明它为何被删）。只断言
	// **代码级**标记消失 —— 把注释一起禁掉等于禁止记录这段历史。

	// configuredBackend 必须只返回 litflow：升级用户配置里可能还留着旧值
	// "script"，读它会让插件找不到任何可用后端，表现为静默不翻译。
	if !strings.Contains(js, `function configuredBackend()`) {
		t.Fatal("configuredBackend 不见了")
	}
	idx := strings.Index(js, "function configuredBackend()")
	body := js[idx:min(idx+240, len(js))]
	if !strings.Contains(body, `return "litflow";`) {
		t.Errorf("configuredBackend 必须无条件返回 litflow（旧值 script 也要当 litflow），实际：\n%s", body)
	}
	if strings.Contains(body, `value === "script"`) {
		t.Error("configuredBackend 仍在区分 script —— 那个后端已经不存在了")
	}
}

// TestPluginMarksFailureVisiblely 后端不可用时必须给条目打可见的失败标签。
//
// 断言的是「调用链存在」：markPendingFailed 存在、它被 drainQueue 在 backend
// 不可用时调用、且它用的是 failed 模式。行为级的验证需要 Zotero API，这里做不到。
func TestPluginMarksFailureVisiblely(t *testing.T) {
	js := readPluginJS(t)

	if !strings.Contains(js, "function markPendingFailed(") {
		t.Fatal("markPendingFailed 不见了 —— 后端不可用时又会退化成只写日志")
	}
	if !strings.Contains(js, `TAG_FAILED = "翻译失败"`) {
		t.Error("缺少「翻译失败」标签常量")
	}
	if !strings.Contains(js, `mode === "failed"`) {
		t.Error("setStatusTag 没有 failed 模式")
	}
	// drainQueue 里必须真的调用它，否则函数存在也没用。
	if !strings.Contains(js, "markPendingFailed(backend.reason)") {
		t.Error("drainQueue 在后端不可用时没有调用 markPendingFailed")
	}
}

// TestPluginSetStatusTagFailedMode setStatusTag 的 failed 模式真的加/删对标签。
// 这条跑的是插件真源码，不是字符串匹配 —— 标签互斥写错（比如同时留「已译」和
// 「失败」）在这里会直接暴露。
func TestPluginSetStatusTagFailedMode(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; setStatusTag 测试跳过")
	}
	js := readPluginJS(t)

	// 切两段：常量与助手（含 isRegular），以及 setStatusTag 本体。
	a0 := strings.Index(js, "var TAG_DONE")
	a1 := strings.Index(js, "function toFile(")
	b0 := strings.Index(js, "function setStatusTag(item, mode)")
	b1 := strings.Index(js, "function parentHasDualAtt(")
	if a0 < 0 || a1 <= a0 || b0 < 0 || b1 <= b0 {
		t.Fatalf("切不出需要的源码区间 (a0=%d a1=%d b0=%d b1=%d)", a0, a1, b0, b1)
	}
	slice := js[a0:a1] + "\n" + js[b0:b1]

	script := `const vm = require('vm');
const src = process.argv[2];
const ctx = { Promise, String, RegExp, parseInt, Object, console };
vm.createContext(ctx);
vm.runInContext(src + '\nthis.setStatusTag = setStatusTag; this.TAGS = {DONE: TAG_DONE, TODO: TAG_TODO, FAILED: TAG_FAILED};', ctx);

function mkItem(tags) {
  const set = new Set(tags);
  return {
    isRegularItem: () => true,
    hasTag: n => set.has(n),
    addTag: n => set.add(n),
    removeTag: n => set.delete(n),
    saveTx: () => Promise.resolve(),
    _tags: () => Array.from(set).sort(),
  };
}

Promise.all([
  (function () { const it = mkItem(['双语已译']); return ctx.setStatusTag(it, 'failed').then(() => it._tags()); })(),
  (function () { const it = mkItem(['翻译失败']); return ctx.setStatusTag(it, 'done').then(() => it._tags()); })(),
  (function () { const it = mkItem(['待翻译']); return ctx.setStatusTag(it, 'failed').then(() => it._tags()); })(),
]).then(r => process.stdout.write(JSON.stringify({ tags: ctx.TAGS, results: r })))
  .catch(e => { console.error(e); process.exit(1); });`

	f := filepath.Join(t.TempDir(), "setstatustag.cjs")
	if err := os.WriteFile(f, []byte(script), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(node, f, slice).CombinedOutput()
	if err != nil {
		t.Fatalf("node 跑 setStatusTag 失败: %v\n%s", err, out)
	}

	var got struct {
		Tags    map[string]string `json:"tags"`
		Results [][]string        `json:"results"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("解析 node 输出失败: %v\n原文: %s", err, out)
	}

	failed := got.Tags["FAILED"]
	done := got.Tags["DONE"]
	// 1) 已译 → failed：必须去掉「已译」加上「失败」，不能两者并存。
	if !contains(got.Results[0], failed) || contains(got.Results[0], done) {
		t.Errorf("已译转失败后应只有 %q，实际 %v", failed, got.Results[0])
	}
	// 2) 失败 → done：必须去掉「失败」。
	if !contains(got.Results[1], done) || contains(got.Results[1], failed) {
		t.Errorf("失败转已译后应只有 %q，实际 %v", done, got.Results[1])
	}
	// 3) 待翻译 → failed：加失败。
	if !contains(got.Results[2], failed) {
		t.Errorf("应加上 %q，实际 %v", failed, got.Results[2])
	}
}

func contains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
