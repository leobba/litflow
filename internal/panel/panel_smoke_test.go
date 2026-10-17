package panel

// 面板前端的冒烟测试。
//
// 为什么需要它：这个会话里 13 次提交有 10 次是修复，而其中至少 5 次修的是**改动
// 自己引入的前端 bug**——`apiRoot` 只加调用没加定义、面板资源没发 Cache-Control
// 导致修好的东西原样复现、`refreshZotero()` 被某次替换吃掉、模型开关只写了一半。
// 同一时期 Go 后端（18,661 行测试）几乎不出事。差别不在难度，在于有没有测试。
//
// 这组断言刻意全部是**静态文本分析 + 一次真实 HTTP 请求**，不引入 Node/npm：
// 面板的全部价值之一就是「go build 出来就是成品」，为测试引入前端构建链会把那条
// 链路重新接回去。

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// stripJSComments 去掉 // 行注释与 /* */ 块注释。
//
// 这不是洁癖：这些断言都在 app.js 里找「某个调用在不在」，而我用来解释修复的注释
// 里恰好写着那个函数名（"refreshZotero 此前根本不在这个函数里"）。于是删掉真正的
// 调用之后，注释仍让断言通过 —— 变异测试当场把这条装饰性断言抓了出来。
// 断言必须看代码，不能看解释代码的文字。
var (
	reJSBlockComment = regexp.MustCompile(`(?s)/\*.*?\*/`)
	reJSLineComment  = regexp.MustCompile(`(?m)//[^\r\n]*`)
)

func stripJSComments(js string) string {
	return reJSLineComment.ReplaceAllString(reJSBlockComment.ReplaceAllString(js, ""), "")
}

func srcJS() string   { return string(appJS) }
func srcHTML() string { return string(indexHTML) }

// ─────────────────────────────────────────────────────────────────────
// A. 引用一致性：app.js 碰到的元素必须在 index.html 里真的存在
// ─────────────────────────────────────────────────────────────────────

// dynamicIDs 运行期才被创建、因而不在 index.html 里的元素。
// 目前是空的：`toasts` 起初被我当成动态创建，其实它就在 index.html 第 1336 行 ——
// 这条豁免本身就是一次误判，而 TestDynamicIDAllowlistIsReal 立刻把它抓了出来。
// 保留这个机制是给将来真正动态创建的容器用的。
var dynamicIDs = map[string]bool{}

var (
	reDollarID = regexp.MustCompile(`\$\('([^']+)'\)`)
	// 有些 id 是**作为字符串参数传给本地助手**的，不会以 $('x') 的形式出现。
	// stat('trStatRun', …) 就是一例：变异测试把 id 改坏后断言仍然通过，才暴露出
	// 这个盲区。助手在这里显式登记，新增同类助手时要一并加进来。
	reHelperID = regexp.MustCompile(`\bstat\('([^']+)'`)
	reGetById  = regexp.MustCompile(`getElementById\("([^"]+)"\)`)
	reHTMLID   = regexp.MustCompile(`\bid="([^"]+)"`)
	reFuncDef  = regexp.MustCompile(`\bfunction\s+([A-Za-z_$][\w$]*)\s*\(`)
	// 三种赋值式定义都要认：`= function`（匿名函数）、`= (…) =>`（带括号参数）、
	// `= x =>`（单参数裸箭头）。
	//
	// 第三种是补上的：`const tipOf = p => …` 原先不被承认为定义，于是 tipOf(p)
	// 被当成「调用了不存在的名字」而误报。改动 rateCell 时就这么被拦了一次 ——
	// 拦得对（新名字确实要有人确认），但原因是工具不全，不是代码有错。
	//
	// 已知局限：这是文本检测，不区分作用域。一个名字在任何地方被赋值过就算
	//「已定义」—— 实测 `const pick = $('btnTrPick')` 恰好让同名的局部箭头函数
	// `const pick = s => …` 免检。要根治得上真正的解析器，收益不值得。
	reAssignFunc = regexp.MustCompile(`\b(?:const|let|var)\s+([A-Za-z_$][\w$]*)\s*=\s*(?:async\s+)?(?:function\b|\([^)]*\)\s*=>|[A-Za-z_$][\w$]*\s*=>)`)
	// RE2 不支持后行断言 (?<!...)，所以先吃掉一个可选的前导字符再判断：
	// 前导为 "." 说明是方法调用，不是本地函数调用。
	reCall = regexp.MustCompile(`(\.?)([A-Za-z_$][\w$]*)\s*\(`)
)

func TestEveryReferencedElementExists(t *testing.T) {
	js, html := srcJS(), srcHTML()

	have := map[string]bool{}
	for _, m := range reHTMLID.FindAllStringSubmatch(html, -1) {
		have[m[1]] = true
	}

	seen := map[string]bool{}
	var missing []string
	for _, re := range []*regexp.Regexp{reDollarID, reGetById, reHelperID} {
		for _, m := range re.FindAllStringSubmatch(js, -1) {
			id := m[1]
			if seen[id] || have[id] || dynamicIDs[id] {
				continue
			}
			seen[id] = true
			missing = append(missing, id)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Fatalf("app.js 引用了 %d 个 index.html 里不存在的元素 id，这些 $('x') 会得到 null "+
			"并在运行期抛错：\n  %s", len(missing), strings.Join(missing, "\n  "))
	}
}

// TestDynamicIDAllowlistIsReal 允许清单不能变成垃圾桶：每个被豁免的 id 都必须
// 真的被创建出来，否则豁免的是个真实缺失。
func TestDynamicIDAllowlistIsReal(t *testing.T) {
	js := srcJS()
	for id := range dynamicIDs {
		if !strings.Contains(js, `id = '`+id+`'`) &&
			!strings.Contains(js, `id="`+id+`"`) &&
			!strings.Contains(js, `id='`+id+`'`) {
			t.Fatalf("允许清单里的 %q 在 app.js 里找不到创建它的地方", id)
		}
	}
}

// ─────────────────────────────────────────────────────────────────────
// B. 未定义调用：冻结基线，新增即失败
// ─────────────────────────────────────────────────────────────────────

// knownGlobals 是「被调用、但本文件不定义」的合法名字：JS 内建、浏览器 API、
// 以及当前实现里确实存在的解构/参数名。
//
// 做法是**基线锁定**：这份清单冻结今天的现状，任何**新增**的未定义调用立刻失败。
// 这正是 `apiRoot` 那一类 bug 的形态——调用点加上了、定义没跟上，而 JS 不会在
// 加载时报错，只在用户点开那个标签页时才炸。
var knownGlobals = map[string]bool{
	"$": true, "Array": true, "Date": true, "Error": true, "FormData": true,
	"Map": true, "Number": true, "Promise": true, "Set": true, "String": true,
	"TextEncoder": true, "URLSearchParams": true, "addEventListener": true,
	"clearInterval": true, "clearTimeout": true, "confirm": true,
	"encodeURIComponent": true, "fetch": true, "isFinite": true, "isNaN": true,
	"matchMedia": true, "open": true, "parseFloat": true, "setInterval": true,
	"setTimeout": true,
	// 下面这些是当前实现里的解构/多声明名，my 正则没有覆盖到。
	"BCH": true, "GF": true, "colorOf": true, "fb": true, "left": true,
	"long": true, "mdProbeOf": true, "normalize": true, "num": true,
	"pick": true, "sizeOf": true, "sz": true, "url": true, "xOf": true, "yOf": true,
}

var keywords = map[string]bool{
	"if": true, "for": true, "while": true, "switch": true, "catch": true,
	"return": true, "typeof": true, "new": true, "function": true, "await": true,
	"throw": true, "else": true, "do": true, "try": true, "case": true,
	"delete": true, "void": true, "in": true, "of": true, "instanceof": true,
	"yield": true, "async": true, "var": true,
}

var (
	reFnParams = regexp.MustCompile(`\bfunction\s*[A-Za-z_$\w]*\s*\(([^)]*)\)`)
	reArrowP   = regexp.MustCompile(`\(([^()]*)\)\s*=>`)
	reCatchP   = regexp.MustCompile(`\bcatch\s*\(\s*([A-Za-z_$][\w$]*)\s*\)`)
	reSingleAr = regexp.MustCompile(`(\.?)([A-Za-z_$][\w$]*)\s*=>`)
	reIdent    = regexp.MustCompile(`^[A-Za-z_$][\w$]*$`)
	reSplit    = regexp.MustCompile(`[,{}\s=]+`)
)

func collectParams(js string) map[string]bool {
	out := map[string]bool{}
	add := func(list string) {
		for _, p := range reSplit.Split(list, -1) {
			if reIdent.MatchString(p) {
				out[p] = true
			}
		}
	}
	for _, m := range reFnParams.FindAllStringSubmatch(js, -1) {
		add(m[1])
	}
	for _, m := range reArrowP.FindAllStringSubmatch(js, -1) {
		add(m[1])
	}
	for _, m := range reCatchP.FindAllStringSubmatch(js, -1) {
		out[m[1]] = true
	}
	for _, m := range reSingleAr.FindAllStringSubmatch(js, -1) {
		if m[1] != "" {
			continue // 前面紧贴着别的标识符或点，不是参数声明
		}
		out[m[2]] = true
	}
	return out
}

func TestNoNewUndefinedCalls(t *testing.T) {
	js := stripJSComments(srcJS())

	defined := map[string]bool{}
	for _, m := range reFuncDef.FindAllStringSubmatch(js, -1) {
		defined[m[1]] = true
	}
	for _, m := range reAssignFunc.FindAllStringSubmatch(js, -1) {
		defined[m[1]] = true
	}
	params := collectParams(js)

	var bad []string
	for _, m := range reCall.FindAllStringSubmatch(js, -1) {
		if m[1] == "." {
			continue // 方法调用 foo.bar()，不是本地函数
		}
		name := m[2]
		if defined[name] || params[name] || keywords[name] || knownGlobals[name] {
			continue
		}
		bad = append(bad, name)
	}
	sort.Strings(bad)
	bad = dedupe(bad)

	if len(bad) > 0 {
		t.Fatalf("app.js 调用了 %d 个既没定义、也不在已知全局里的名字。"+
			"这类 bug 不会在加载时报错，只在用户点开对应标签页时才炸"+
			"（apiRoot 就是这么丢的）：\n  %s",
			len(bad), strings.Join(bad, "\n  "))
	}
}

func dedupe(in []string) []string {
	out := make([]string, 0, len(in))
	for i, s := range in {
		if i == 0 || in[i-1] != s {
			out = append(out, s)
		}
	}
	return out
}

// ─────────────────────────────────────────────────────────────────────
// C. 视图与导航一致：TITLES 里的每一项都要有对应 section，反之亦然
// ─────────────────────────────────────────────────────────────────────

var (
	reTitleKey = regexp.MustCompile(`TITLES\s*=\s*\{([^}]*)\}`)
	reViewSec  = regexp.MustCompile(`<section class="view" id="view-([a-z0-9]+)"`)
)

func TestViewsMatchTitles(t *testing.T) {
	js, html := srcJS(), srcHTML()

	m := reTitleKey.FindStringSubmatch(js)
	if m == nil {
		t.Fatal("app.js 里找不到 TITLES 定义")
	}
	titleKeys := map[string]bool{}
	for _, kv := range strings.Split(m[1], ",") {
		parts := strings.SplitN(kv, ":", 2)
		if len(parts) < 2 {
			continue
		}
		k := strings.Trim(strings.TrimSpace(parts[0]), `'"`)
		if k != "" {
			titleKeys[k] = true
		}
	}

	sectionKeys := map[string]bool{}
	for _, mm := range reViewSec.FindAllStringSubmatch(html, -1) {
		sectionKeys[mm[1]] = true
	}

	if len(titleKeys) == 0 || len(sectionKeys) == 0 {
		t.Fatalf("解析失败：TITLES %d 项，section %d 个", len(titleKeys), len(sectionKeys))
	}

	for k := range titleKeys {
		if !sectionKeys[k] {
			t.Errorf("TITLES 里有 %q，但 index.html 没有 id=\"view-%s\" 的 section —— 点进去会是空白页", k, k)
		}
	}
	for k := range sectionKeys {
		if !titleKeys[k] {
			t.Errorf("index.html 有 view-%s，但 TITLES 里没有它 —— 标题会显示 undefined", k)
		}
	}
}

// TestSectionsBalanced 标签配平。手工改这个文件很容易多删一个 </section>，
// 而那会让后面所有视图塌进前一个里。
func TestSectionsBalanced(t *testing.T) {
	html := srcHTML()
	open := strings.Count(html, "<section")
	closed := strings.Count(html, "</section>")
	if open != closed {
		t.Fatalf("<section %d 个，</section> %d 个", open, closed)
	}
	if open == 0 {
		t.Fatal("一个 section 都没有")
	}
}

// ─────────────────────────────────────────────────────────────────────
// D. data-* 选择器必须有对应的承载体
// ─────────────────────────────────────────────────────────────────────

var (
	reHTMLComment = regexp.MustCompile(`(?s)<!--.*?-->`)

	reDataSel  = regexp.MustCompile(`querySelectorAll\('\[(data-[a-z-]+)\]'\)`)
	reDataEmit = func(attr string) *regexp.Regexp {
		// 生成侧可能是 data-x="，也可能在模板字符串里拼接
		return regexp.MustCompile(`\b` + regexp.QuoteMeta(attr) + `=`)
	}
)

func TestDataSelectorsHaveProducers(t *testing.T) {
	js, html := srcJS(), srcHTML()
	seen := map[string]bool{}
	for _, m := range reDataSel.FindAllStringSubmatch(js, -1) {
		attr := m[1]
		if seen[attr] {
			continue
		}
		seen[attr] = true
		// 选择器自己那一处不算「承载体」，所以在去掉选择器之后再找
		body := strings.ReplaceAll(js, `querySelectorAll('[`+attr+`]')`, "")
		if reDataEmit(attr).MatchString(body) || reDataEmit(attr).MatchString(html) {
			continue
		}
		t.Errorf("用了 %q 选择器，但没有任何地方生成这个属性 —— 那些按钮永远不会绑上事件", attr)
	}
	if len(seen) == 0 {
		t.Fatal("一个 data-* 选择器都没解析到，正则可能失效了")
	}
}

// ─────────────────────────────────────────────────────────────────────
// E. 回归护栏：这一轮真实坏过的两件事
// ─────────────────────────────────────────────────────────────────────

// TestEnterEnvCallsRefreshEnv 进「环境检测」页时必须真的去拉一次状态。
//
// 这条守的是本仓库丢过好几次的那个形态：视图切换里忘了调刷新函数，页面就永远
// 停在「检测中…」。当年 refreshZotero() 被某次替换吃掉，翻译页那张卡片一直是
// 空白，而所有测试都是绿的 —— 因为没有任何断言管「进页面时该调什么」。
//
// 断言「被调用」（后面紧跟 `(`），不是「出现过」：后者会被解释性注释满足，
// 而注释写着「这里应该调 refreshEnv()」恰恰是最容易发生的情形。
func TestEnterEnvCallsRefreshEnv(t *testing.T) {
	js := stripJSComments(srcJS())

	if !strings.Contains(js, "async function refreshEnv(") {
		t.Fatal("找不到 refreshEnv —— 环境检测页会一直停在初始状态")
	}

	i := strings.Index(js, "function go(")
	if i < 0 {
		t.Fatal("找不到 go()")
	}
	// 终点用行首的 `}`（函数体收尾）。之前用 "\n  }" 会先撞上 go() 内部
	// `if (tabs) {…}` 的收尾，切片在刷新派发之前就断了 —— 断言于是误报「没调用」。
	// 第一版就是这么红的；红得对（它逼我去看真代码），但理由是我切错了。
	end := strings.Index(js[i:], "\n}")
	if end < 0 {
		t.Fatal("go() 没有找到收尾")
	}
	body := js[i : i+end]
	if !strings.Contains(body, "refreshEnv(") {
		t.Errorf("go() 里没有调用 refreshEnv() —— 点进「环境检测」不会重新检测，\n"+
			"用户看到的永远是上一次(或初始)的结论。\n  实际 go() 片段：\n%s", body)
	}
}

// TestLoadTranslateRefreshesZotero 直接钉住一个真实回归：
// refreshZotero() 曾在某次编辑里从 loadTranslate() 消失，卡片于是永远停在
// 「检测中…」，而每次人工测试我都点了刷新按钮，所以没发现。
func TestLoadTranslateRefreshesZotero(t *testing.T) {
	js := srcJS()
	i := strings.Index(js, "async function loadTranslate(")
	if i < 0 {
		t.Fatal("找不到 loadTranslate")
	}
	end := strings.Index(js[i:], "\n}")
	if end < 0 {
		end = 900
	}
	body := stripJSComments(js[i : i+int(end)])
	// 要求的是「被调用」（后面紧跟 ( 或 ,），不是「出现过」——后者会被注释满足。
	for _, want := range []string{"refreshZotero", "loadTranslateModels", "refreshTranslate"} {
		called := strings.Contains(body, want+"(") || strings.Contains(body, want+",")
		if !called {
			t.Errorf("loadTranslate 没有调用 %s() —— 那张卡片/那个列表会一直停在初始状态", want)
		}
	}
}

// ─────────────────────────────────────────────────────────────────────
// F. 真实 HTTP：缓存头与安全头
// ─────────────────────────────────────────────────────────────────────

// TestStaticAssetsAreNotCached 钉住第二个真实回归：面板资源是 go:embed 进
// 二进制的，每次重建内容变而 URL 不变。没有 Cache-Control 时浏览器按启发式
// 缓存，于是「改好了却看不到变化」—— 一个前端 bug 修完仍然原样复现。
func TestStaticAssetsAreNotCached(t *testing.T) {
	p := &Panel{mux: http.NewServeMux()}
	p.mux.HandleFunc("GET /panel/", p.index)
	p.mux.HandleFunc("GET /panel/app.js", p.appScript)

	for _, path := range []string{"/panel/", "/panel/app.js"} {
		rr := httptest.NewRecorder()
		p.mux.ServeHTTP(rr, httptest.NewRequest("GET", path, nil))
		if rr.Code != http.StatusOK {
			t.Fatalf("%s 返回 %d", path, rr.Code)
		}
		cc := rr.Header().Get("Cache-Control")
		if !strings.Contains(cc, "no-cache") {
			t.Errorf("%s 的 Cache-Control 是 %q —— 面板资源必须 no-cache，"+
				"否则重建后的前端不会生效", path, cc)
		}
		if rr.Header().Get("Content-Security-Policy") == "" {
			t.Errorf("%s 没有 CSP 头", path)
		}
		if rr.Body.Len() == 0 {
			t.Errorf("%s 响应体为空", path)
		}
	}
}

// TestNoInlineEventHandlers CSP 用的是 script-src 'self'（不含 unsafe-inline），
// 所以页面里出现 onclick="..." 这类内联处理器会被浏览器直接拦掉，而且只在
// 控制台报错、界面上什么都不发生。这条断言把它挡在提交前。
// TestEveryJobStateHasAnAction 每个终态都要有出口。
//
// 为什么值得单独一条：失败是翻译任务最常见的终态，而它长时间是**唯一没有任何
// 按钮**的那个 —— 用户想再试一次，得滚回输入框重新粘一遍完整路径。这类缺失不会
// 被任何语法或引用检查发现（代码完全合法），只能靠一条「状态 × 动作」的完备性
// 断言守住。
//
// 这是静态的结构断言，不是行为测试：它证明「failed 分支存在」，不证明「渲染出来
// 真的有一个按钮」。后者需要 DOM，本仓库没有 JS 测试环境 —— 这个边界要说清楚，
// 否则它看起来比实际更强。
func TestEveryJobStateHasAnAction(t *testing.T) {
	js := stripJSComments(srcJS())

	// 状态清单以 trStateChip 的映射表为准 —— 单一来源，不另抄一份。
	start := strings.Index(js, "const map = {")
	if start < 0 {
		t.Fatal("找不到 trStateChip 的状态映射表")
	}
	end := strings.Index(js[start:], "};")
	if end < 0 {
		t.Fatal("状态映射表没有闭合")
	}
	block := js[start : start+end]
	states := regexp.MustCompile(`(\w+):\s*\[`).FindAllStringSubmatch(block, -1)
	if len(states) < 5 {
		t.Fatalf("只解析出 %d 个状态，映射表格式可能变了", len(states))
	}

	// 动作选择区：从 retry 的定义到表格行的 return 之间。
	actStart := strings.Index(js, "const retry =")
	actEnd := strings.Index(js, "return `<tr class=")
	if actStart < 0 || actEnd < 0 || actEnd <= actStart {
		t.Fatal("找不到动作选择区")
	}
	act := js[actStart:actEnd]

	// queued / running 由 busy 统一覆盖；done 走兜底 else（显示「复制路径」）；
	// 其余每个状态都必须有显式分支。
	//
	// 把 done 单独列出来而不是让它算作「有分支」，是因为它确实没有 —— 它靠的是
	// 最后一个 else。写成例外比放宽成「出现 else 就算过」更精确：后者会让
	// 「failed 分支被删掉」也通过，而那正是这条断言要防的事。
	for _, m := range states {
		st := m[1]
		if st == "queued" || st == "running" {
			if !strings.Contains(act, "busy") {
				t.Errorf("状态 %q 依赖 busy，但动作区没有 busy", st)
			}
			continue
		}
		if st == "done" {
			continue // 兜底 else → 复制路径
		}
		if !strings.Contains(act, "'"+st+"'") {
			t.Errorf("状态 %q 在动作区没有分支 —— 这个状态的任务将没有任何可用按钮", st)
		}
	}

	// 必须有一个兜底 else，否则「完成」这类正常状态会拿到 undefined。
	if !regexp.MustCompile(`\}\s*else\s*\{`).MatchString(act) {
		t.Error("动作选择缺少兜底 else 分支")
	}
}

// TestEveryClassNameHasStyling HTML 里用到的每个 class 都必须在内联样式表里
// 至少有一条规则。
//
// 这条守的是一个**真实存在过、而且没人发现**的问题：翻译页的队列表写的是
// `class="tbl"`，而整份样式表里 `.tbl` 一条规则都没有（只有 `.tbl-wrap`）——
// 于是那张表一直是浏览器默认样式，紧贴、无分隔、数字不对齐。没有任何报错，
// 也拦不住任何测试，因为一个**完全不生效的 class 与一个生效的 class 在源码里
// 长得一模一样**。
//
// 顺带也守住了这个仓库反复踩的另一个形态：改了 class 名却没改 CSS（或反过来）。
//
// 边界：只查内联 `<style>` 块。这个项目的前端是 go:embed 的单文件、没有任何
// 外部样式表（外链本身有别的断言在挡），所以内联块就是全部样式。
func TestEveryClassNameHasStyling(t *testing.T) {
	html := srcHTML()

	styleStart := strings.Index(html, "<style>")
	styleEnd := strings.Index(html, "</style>")
	if styleStart < 0 || styleEnd <= styleStart {
		t.Fatal("找不到内联 <style> 块")
	}
	css := html[styleStart:styleEnd]
	body := html[styleEnd:]

	usedSet := map[string]bool{}
	for _, m := range regexp.MustCompile(`class="([^"]*)"`).FindAllStringSubmatch(body, -1) {
		for _, c := range strings.Fields(m[1]) {
			usedSet[c] = true
		}
	}
	if len(usedSet) < 50 {
		t.Fatalf("只解析出 %d 个 class，解析可能失效了（预期 100 上下）", len(usedSet))
	}

	// CSS 里的 class 选择器：`.name`（排除属性选择器里的点之类，简单起见只认
	// 点后跟合法标识符）。这段正则也覆盖 `.a .b` / `.a:hover` 这类复合选择器。
	definedSet := map[string]bool{}
	for _, m := range regexp.MustCompile(`\.([A-Za-z][\w-]*)`).FindAllStringSubmatch(css, -1) {
		definedSet[m[1]] = true
	}

	var missing []string
	for c := range usedSet {
		if !definedSet[c] {
			missing = append(missing, c)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Fatalf("%d 个 class 在 HTML 里用了，但样式表里没有任何规则 —— "+
			"它们不会报错，只会让那部分界面按浏览器默认样式渲染："+
			"（历史上 `.tbl` 就是这么让翻译队列表裸奔的）\n  %s",
			len(missing), strings.Join(missing, "\n  "))
	}
}

func TestNoInlineEventHandlers(t *testing.T) {
	// **先剥掉 HTML 注释**：注释里出现这个属性的字面量是合法的（比如解释为什么
	// 删掉了它），而朴素的子串搜索会把它当成真处理器。这条判断标准的变化本身
	// 就抓到过一次假阳性 —— 我写注释解释删除原因时触发了自己的测试。
	html := reHTMLComment.ReplaceAllString(srcHTML(), "")
	// 允许 <input ... onchange= 这类？不允许——本项目的 CSP 不允许内联。
	for _, ev := range []string{"onclick=", "onchange=", "onsubmit=", "oninput=", "onkeydown="} {
		if strings.Contains(html, `"`+ev) || strings.Contains(html, " "+ev+`"`) {
			// 缩窄到「在标签内作为属性出现」：形如  onclick="foo()"
			re := regexp.MustCompile(`(?s)\b` + ev + `"[^"]*"`)
			if re.MatchString(html) {
				t.Errorf("页面里有内联事件处理器 %s，CSP 的 script-src 'self' 会拦掉它", ev)
			}
		}
	}
}
