package translate

// 用户术语表的测试。
//
// 这组断言的重点是「坏表要早说」：术语表是**可选**功能，配错了却让翻译失败，
// 比没有这个功能更糟。所以每种坏法都必须被 CheckGlossaries 认出来，
// 而不是留到 pdf2zh 深处抛一段陌生堆栈。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeCSV(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestCheckGlossariesGood(t *testing.T) {
	dir := t.TempDir()
	p := writeCSV(t, dir, "农化.csv", "source,target,tgt_lng\nnanocapsule,纳米胶囊\n"+
		"tannic acid,单宁酸,zh-CN\nmetal-organic framework,金属有机框架\n")

	res := CheckGlossaries([]string{p})
	if len(res) != 1 {
		t.Fatalf("期望 1 项结果，实际 %d", len(res))
	}
	if !res[0].OK {
		t.Fatalf("合法术语表被判定不可用: %s", res[0].Error)
	}
	if res[0].Terms != 3 {
		t.Errorf("词条数 = %d，期望 3", res[0].Terms)
	}
}

// TestCheckGlossariesTwoColumns 不带 tgt_lng 的两列格式也合法（babeldoc 允许省略）。
func TestCheckGlossariesTwoColumns(t *testing.T) {
	dir := t.TempDir()
	p := writeCSV(t, dir, "g.csv", "source,target\npesticide,农药\n")
	res := CheckGlossaries([]string{p})
	if !res[0].OK {
		t.Fatalf("两列格式应当合法: %s", res[0].Error)
	}
	if res[0].Terms != 1 {
		t.Errorf("词条数 = %d，期望 1", res[0].Terms)
	}
}

func TestCheckGlossariesRejectsBadInput(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		name string
		file string
		body string
		want string // 错误里应出现的词
	}{
		{"缺 source 列", "nosrc.csv", "关键词,target\nx,y\n", "source"},
		{"缺 target 列", "notgt.csv", "source,目标\nx,y\n", "target"},
		{"空文件", "empty.csv", "", "空"},
		{"只有表头没有词条", "headeronly.csv", "source,target\n", "词条"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := writeCSV(t, dir, c.file, c.body)
			res := CheckGlossaries([]string{p})
			if res[0].OK {
				t.Fatal("应当判为不可用")
			}
			if !strings.Contains(res[0].Error, c.want) {
				t.Errorf("错误信息里应提到 %q，实际 %q", c.want, res[0].Error)
			}
		})
	}
}

func TestCheckGlossariesRejectsMissingAndDir(t *testing.T) {
	dir := t.TempDir()
	res := CheckGlossaries([]string{filepath.Join(dir, "nope.csv")})
	if res[0].OK || !strings.Contains(res[0].Error, "不存在") {
		t.Errorf("不存在的文件应明确说「不存在」，实际 %q", res[0].Error)
	}

	res2 := CheckGlossaries([]string{dir})
	if res2[0].OK || !strings.Contains(res2[0].Error, "目录") {
		t.Errorf("传目录应明确说「目录」，实际 %q", res2[0].Error)
	}
}

func TestCheckGlossariesRejectsWrongExtension(t *testing.T) {
	dir := t.TempDir()
	p := writeCSV(t, dir, "g.txt", "source,target\nx,y\n")
	res := CheckGlossaries([]string{p})
	if res[0].OK {
		t.Fatal("非 .csv 扩展名应被拒 —— babeldoc 只按 CSV 解析")
	}
	if !strings.Contains(res[0].Error, "csv") {
		t.Errorf("错误里应提到 csv，实际 %q", res[0].Error)
	}
}

// TestGlossaryArgJoinsWithComma pdf2zh 用逗号切分这一项（high_level.py 里
// settings.translation.glossaries.split(",")），所以多个表必须用逗号连接。
func TestGlossaryArgJoinsWithComma(t *testing.T) {
	got, err := glossaryArg([]string{`C:\a\g1.csv`, `C:\a\g2.csv`})
	if err != nil {
		t.Fatal(err)
	}
	if got != `C:\a\g1.csv,C:\a\g2.csv` {
		t.Fatalf("拼接结果 = %q", got)
	}
}

// TestGlossaryArgRejectsCommaInPath 路径里的逗号会被当成分隔符切成两半，
// 后果是 pdf2zh 报「文件不存在」而用户看着一个明明存在的文件。
func TestGlossaryArgRejectsCommaInPath(t *testing.T) {
	if _, err := glossaryArg([]string{`C:\a\g,1.csv`}); err == nil {
		t.Fatal("含逗号的路径必须被拒，否则会被 pdf2zh 切成两段")
	}
}

func TestGlossaryArgEmpty(t *testing.T) {
	got, err := glossaryArg(nil)
	if err != nil || got != "" {
		t.Fatalf("空列表应返回空串且无错，实际 %q / %v", got, err)
	}
}

// TestBuildArgsIncludesGlossaries 参数必须真的出现在命令行上，
// 而且要用逗号拼接的单个字符串。
func TestBuildArgsIncludesGlossaries(t *testing.T) {
	dir := t.TempDir()
	g1 := writeCSV(t, dir, "a.csv", "source,target\nx,y\n")
	g2 := writeCSV(t, dir, "b.csv", "source,target\np,q\n")

	cfg := DefaultConfig()
	cfg.Glossaries = []string{g1, g2}
	q := testQueue(t, cfg)
	job := &Job{Source: `C:\p\a.pdf`, OutputDir: `C:\p`, Model: "m", QPS: "2"}

	args := q.buildArgs(job)
	joined := strings.Join(args, " ")
	want := "--glossaries " + g1 + "," + g2
	if !strings.Contains(joined, want) {
		t.Fatalf("命令行缺少 %q\n  实际: %s", want, joined)
	}
}

func TestBuildArgsOmitsGlossariesWhenEmpty(t *testing.T) {
	q := testQueue(t, DefaultConfig())
	job := &Job{Source: `C:\p\a.pdf`, OutputDir: `C:\p`, Model: "m", QPS: "2"}
	if strings.Contains(strings.Join(q.buildArgs(job), " "), "--glossaries") {
		t.Error("没配术语表时不该传 --glossaries")
	}
}
