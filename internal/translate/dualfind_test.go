package translate

// findDualPDFs 的测试 —— 它决定「这次翻译到底有没有产出」。
//
// 这个函数判错的方向只有一种：**假阳性**（把不存在的东西当成产出）。而它的后果
// 恰好是本项目最怕的那一类 —— 静默成功：
//
//   - runOne 看到 len(duals) > 0 就不判 produced_nothing，于是「什么都没译」
//     被报成 done；
//   - Submit 的 skip_existing 认为「已有译文」，直接跳过，根本不翻；
//   - 插件拿到那个路径，把它当双语件挂到条目上，用户看到的是**没翻译的原件**。
//
// 下面第一个用例就是实测数据的形状：论文标题里含 "dual" 的词（
// "dual-stimuli-responsive pesticides"），而双语件按设计生成在**源文件同一个
// 目录**里 —— 于是源文件本身和它的产出躺在同一个目录，且都含 "dual"。

import (
	"os"
	"path/filepath"
	"testing"
)

// TestFindDualPDFsSourceNamedLikeADual 源文件名含 "dual" 时，源文件本身绝不能被
// 当成双语产出。
func TestFindDualPDFsSourceNamedLikeADual(t *testing.T) {
	dir := t.TempDir()
	// 真实标题的形状（来自 data\translated 里的实测文件）。
	base := "Liang 等 - 2022 - Pectin functionalized metal-organic frameworks as dual-stimuli-responsive carriers to improve the pe"
	src := filepath.Join(dir, base+".pdf")
	if err := os.WriteFile(src, []byte("%PDF-1.4 original"), 0o644); err != nil {
		t.Fatal(err)
	}

	// 只有源文件、还没有任何产出。
	got := findDualPDFs(dir, src)
	if len(got) != 0 {
		t.Fatalf("没有任何产出，却报出了 %d 个「双语件」：%v\n"+
			"源文件名里含 dual，而产出与源同目录 —— 这就是把原件当产出的那条路。",
			len(got), got)
	}

	// 真产出出现后，只能报出那一个。
	dual := filepath.Join(dir, base+".no_watermark.zh-CN.dual.pdf")
	if err := os.WriteFile(dual, []byte("%PDF-1.4 dual"), 0o644); err != nil {
		t.Fatal(err)
	}
	got = findDualPDFs(dir, src)
	if len(got) != 1 || filepath.Base(got[0]) != filepath.Base(dual) {
		t.Fatalf("应只报出真正的双语件，实际 %v", got)
	}
}

// TestFindDualPDFsIgnoresOtherPDFsInFolder 同目录的其它 PDF（原始件、别的论文、
// 上次残留）都不能算产出。
func TestFindDualPDFsIgnoresOtherPDFsInFolder(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "paper.pdf")
	os.WriteFile(src, []byte("%PDF"), 0o644)
	// 同前缀但不是产出的文件
	os.WriteFile(filepath.Join(dir, "paper-supplement.pdf"), []byte("%PDF"), 0o644)
	os.WriteFile(filepath.Join(dir, "paper.notes.pdf"), []byte("%PDF"), 0o644)
	// 别的论文的产出
	os.WriteFile(filepath.Join(dir, "other.no_watermark.zh-CN.dual.pdf"), []byte("%PDF"), 0o644)

	if got := findDualPDFs(dir, src); len(got) != 0 {
		t.Fatalf("这些都不是本论文的产出，不该报出：%v", got)
	}
}

// TestFindDualPDFsAcceptsEveryRealOutputShape pdf2zh 实际会产出的几种文件名形态
// 都必须被认出来 —— 认漏了会把成功的翻译报成 produced_nothing。
func TestFindDualPDFsAcceptsEveryRealOutputShape(t *testing.T) {
	shapes := []string{
		"paper.no_watermark.zh-CN.dual.pdf", // pdf2zh_next 的默认形态（实测）
		"paper.dual.pdf",
		"paper-dual.pdf",
		"paper.no_watermark.zh-CN.dual.pdf",
	}
	for _, name := range shapes {
		dir := t.TempDir()
		src := filepath.Join(dir, "paper.pdf")
		os.WriteFile(src, []byte("%PDF"), 0o644)
		os.WriteFile(filepath.Join(dir, name), []byte("%PDF"), 0o644)
		got := findDualPDFs(dir, src)
		if len(got) != 1 {
			t.Errorf("%q 是合法产出形态，应被认出，实际 %v", name, got)
		}
	}
}

// TestFindDualPDFsSideBySide pdf2zh 的对照模式产出 -side_by_side.pdf。
func TestFindDualPDFsSideBySide(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "paper.pdf")
	os.WriteFile(src, []byte("%PDF"), 0o644)
	os.WriteFile(filepath.Join(dir, "paper-side_by_side.pdf"), []byte("%PDF"), 0o644)
	if got := findDualPDFs(dir, src); len(got) != 1 {
		t.Fatalf("对照模式产出应被认出，实际 %v", got)
	}
}
