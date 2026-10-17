package translate

// 跨组件一致性：网关的 isDualOutputName 与插件 dualsync.js 的 isDualFilename
// 必须对同一个文件名给出**相同**判定。
//
// 为什么值得单独一条：这两个函数分居 Go 和 JS、各自演化，没有任何机制保证它们
// 一致。而两个方向的不一致都是静默的：
//
//	网关认、插件不认 → 翻译完成但 Zotero 里永远挂不上附件
//	插件认、网关不认 → 成功的翻译被记成 produced_nothing
//
// 实测就出过前一类问题的一个变体：网关当时只判「文件名含 dual」，于是标题里带
// "dual" 的论文（"... as dual-stimuli-responsive carriers ..."）会把**源文件
// 自己**当成产出；同时它认不出 -side_by_side.pdf，对照模式翻译成功也会被报成
// 没产出。插件那边四种形态一直是齐的 —— 落后的那一侧没有任何信号告诉你它落后了。
//
// 做法：同一张表，Go 侧直接断言，JS 侧用 node 跑真源码，两边结果逐项比对。

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// dualNameCases 一份文件名清单，两侧都拿它跑。
//
// dual=true 表示「这是 pdf2zh 的双语产出」。注意最后一组：源文件名里含 dual，
// 但它不是产出 —— 这正是靠「含 dual」判定的写法会翻车的地方。
var dualNameCases = []struct {
	name string
	dual bool
}{
	// pdf2zh_next 的默认形态（实测文件名）
	{"paper.no_watermark.zh-CN.dual.pdf", true},
	{"Liang 等 - 2022 - dual-stimuli-responsive carriers.no_watermark.zh-CN.dual.pdf", true},
	// 其它已知产出形态
	{"paper.dual.pdf", true},
	{"paper-dual.pdf", true},
	{"paper-side_by_side.pdf", true},
	// 大小写不应影响判定
	{"PAPER.NO_WATERMARK.ZH-CN.DUAL.PDF", true},
	// —— 以下都不是产出 ——
	{"paper.pdf", false},
	{"paper-supplement.pdf", false},
	{"paper.notes.pdf", false},
	// 源文件：名字里含 dual，但它是原文不是译文
	{"Liang 等 - 2022 - dual-stimuli-responsive carriers.pdf", false},
	{"dual.pdf", false},
	{"paper.xdualx.pdf", false},
	// 单语件不该算双语产出（我们传 --no-mono，理论上不会出现）
	{"paper.no_watermark.zh-CN.mono.pdf", false},
	{"", false},
}

func TestDualNameGoSide(t *testing.T) {
	for _, c := range dualNameCases {
		if got := isDualOutputName(c.name); got != c.dual {
			t.Errorf("isDualOutputName(%q) = %v，期望 %v", c.name, got, c.dual)
		}
	}
}

// TestDualNameAgreesWithPlugin 网关与插件对同一批文件名的判定必须一致。
func TestDualNameAgreesWithPlugin(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; 跨组件一致性检查跳过")
	}
	src, err := os.ReadFile(filepath.Join("..", "..", "plugin", "content", "dualsync.js"))
	if err != nil {
		t.Fatalf("读不到插件源码: %v", err)
	}
	js := string(src)

	// 只切这两个纯函数。插件是 IIFE，顶部 `if (typeof Zotero === "undefined") return;`，
	// 整个文件拿进 vm 会直接返回；切函数体才能测到真逻辑。
	start := indexOf(js, "function isDualFilename(")
	end := indexOf(js, "function basename(")
	if start < 0 || end <= start {
		t.Fatalf("在插件源码里找不到 isDualFilename..basename 区间")
	}

	names, err := json.Marshal(func() []string {
		out := make([]string, 0, len(dualNameCases))
		for _, c := range dualNameCases {
			out = append(out, c.name)
		}
		return out
	}())
	if err != nil {
		t.Fatal(err)
	}

	script := `const vm = require('vm');
const src = process.argv[2];
const names = JSON.parse(process.argv[3]);
const ctx = { String, RegExp, console };
vm.createContext(ctx);
vm.runInContext(src + '\nthis.f = isDualFilename;', ctx);
process.stdout.write(JSON.stringify(names.map(n => ctx.f(n))));`

	f := filepath.Join(t.TempDir(), "dualname.cjs")
	if err := os.WriteFile(f, []byte(script), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(node, f, js[start:end], string(names)).CombinedOutput()
	if err != nil {
		t.Fatalf("node 跑插件函数失败: %v\n%s", err, out)
	}
	var pluginVerdicts []bool
	if err := json.Unmarshal(out, &pluginVerdicts); err != nil {
		t.Fatalf("解析 node 输出失败: %v\n原文: %s", err, out)
	}
	if len(pluginVerdicts) != len(dualNameCases) {
		t.Fatalf("插件返回 %d 项，期望 %d 项", len(pluginVerdicts), len(dualNameCases))
	}

	for i, c := range dualNameCases {
		goVerdict := isDualOutputName(c.name)
		jsVerdict := pluginVerdicts[i]
		if goVerdict != jsVerdict {
			t.Errorf("两侧判定不一致：%q\n  网关=%v  插件=%v\n"+
				"这种分歧是静默的 —— 要么 Zotero 挂不上附件，要么成功的翻译被报成没产出",
				c.name, goVerdict, jsVerdict)
		}
	}
}

// indexOf 是 strings.Index 的小包装，只为让上面的切片逻辑读起来干净。
func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
