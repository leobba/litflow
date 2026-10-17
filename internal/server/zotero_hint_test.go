package server

// Zotero 状态提示的测试。
//
// 为什么单独测这个：提示词是用户唯一能读到的诊断，而它描述的是一个真实存在
// 的错位 —— Zotero 运行中、插件清单里没有该插件，**而菜单里可能还能点**。
//
// 实测被问过一次：「zotero 的插件里 sync 插件消失了，但菜单里仍然可以点击
// 翻译选中条目，而且 litflow 还接收了这个翻译」。那是 bootstrap 插件正常的行为
// （startup() 跑过的作用域活在窗口里，直到重启），但面板当时只说「还没有这个
// 插件」，读起来像自相矛盾。
//
// 提示词的措辞会随 UI 调整，所以断言**语义**而不是逐字：
//   - 运行中但未安装 → 必须解释「菜单可能仍可用」这件事，并说清检测读的是
//     清单、描述的是重启后的状态
//   - 已装未启用 → 必须给出「去哪启用 + 要重启」（Zotero 里这一种和「没装」
//     长得一模一样，都是没有菜单）

import (
	"strings"
	"testing"
)

func TestHintForZoteroRunningButNotInstalledExplainsStaleScope(t *testing.T) {
	got := hintForZotero(true, true, false, false)

	// 必须说清本检测描述的是**重启之后**的状态。
	//
	// 断言要精确到这个词组。第一版只查「重启」，而文案里另有一句「然后重启
	// Zotero」—— 把关键那句删掉，断言照样通过。变异测试当场拆穿了它。
	if !strings.Contains(got, "重启之后") {
		t.Errorf("提示必须说清本检测描述的是**重启之后**的状态，实际：%s", got)
	}
	if !strings.Contains(got, "插件清单") {
		t.Errorf("提示必须点明检测读的是插件清单（而不是当下活着的代码），实际：%s", got)
	}
	// 必须解释「现在还能点」这件事，因为那正是用户的困惑点。
	// 「仍然能点」与「上一次加载」是两个独立的事实，缺一个都说不通。
	for _, want := range []string{"仍然能点", "上一次加载"} {
		if !strings.Contains(got, want) {
			t.Errorf("提示必须解释菜单为何仍可用（含 %q），实际：%s", want, got)
		}
	}
	// 必须给出下一步动作。
	if !strings.Contains(got, "Install Add-on From File") {
		t.Errorf("提示必须给出安装路径，实际：%s", got)
	}
}

// TestHintForZoteroInstalledButInactive 装了但没启用。
// 这一种在 Zotero 里和「没装」看起来一样（都没有菜单），所以提示必须给出
// 唯一的区分动作：去插件列表里启用。
func TestHintForZoteroInstalledButInactive(t *testing.T) {
	got := hintForZotero(true, true, true, false)
	for _, want := range []string{"未启用", "插件", "重启"} {
		if !strings.Contains(got, want) {
			t.Errorf("提示必须提到 %q，实际：%s", want, got)
		}
	}
}

// TestHintForZoteroHealthy 正常状态不该夹带故障排查 —— 那会让健康用户以为出了问题。
func TestHintForZoteroHealthy(t *testing.T) {
	got := hintForZotero(true, true, true, true)
	if strings.Contains(got, "重启") {
		t.Errorf("已安装且启用时不该要求重启，实际：%s", got)
	}
	if !strings.Contains(got, "Dual Sync") {
		t.Errorf("正常状态应告诉用户去哪点，实际：%s", got)
	}
}

// TestHintForZoteroMissingPackage 找不到 xpi 时要说清放哪。
func TestHintForZoteroMissingPackage(t *testing.T) {
	for _, running := range []bool{true, false} {
		got := hintForZotero(running, false, false, false)
		if !strings.Contains(got, pluginFileName) {
			t.Errorf("提示必须给出文件名 %q，实际：%s", pluginFileName, got)
		}
		if !strings.Contains(got, "同目录") {
			t.Errorf("提示必须说明放在哪里，实际：%s", got)
		}
	}
}
