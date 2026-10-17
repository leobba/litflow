package server

// pdf2zh 探测接口的白名单测试。
//
// 这条接口是「你给 URL 我就替你去 HEAD」。没有白名单它就是个 SSRF 跳板：任何能
// 访问面板的人都可以拿它探内网（`http://127.0.0.1:6379` 之类），或者把服务器当
// 免费代理用。
//
// 所以判据必须**剥掉一个已知前缀后逐字全等**，而不是「看起来像 GitHub」：
// 前缀匹配会放过 `https://ghfast.top/https://github.com/<别人的仓库>`。
// 下面每一条「必须拒绝」的用例，都是某一种具体的绕过尝试。

import (
	"strings"
	"testing"
)

func TestPdf2zhProbeURLAllowed(t *testing.T) {
	allow := []string{
		pdf2zhAssetURL,
		"https://ghfast.top/" + pdf2zhAssetURL,
		"https://ghproxy.net/" + pdf2zhAssetURL,
		"https://gh-proxy.com/" + pdf2zhAssetURL,
	}
	for _, u := range allow {
		if !pdf2zhProbeURLAllowed(u) {
			t.Errorf("应当允许：%s", u)
		}
	}

	deny := []struct{ url, why string }{
		{"", "空 URL"},
		{"http://127.0.0.1:6379/", "内网地址 —— 典型 SSRF"},
		{"https://example.com/", "任意外部站点"},
		{"https://github.com/leobba/litflow/releases/download/v1.13.0/dualsync@zotero.local.xpi",
			"本项目其它资产也不行 —— 白名单只放行这一个包"},
		{"https://ghfast.top/https://github.com/attacker/repo/releases/download/x/y.zip",
			"加速前缀 + 别人的仓库（前缀匹配会放过这条）"},
		{"https://ghfast.top/http://127.0.0.1:7863/health",
			"加速前缀 + 内网"},
		{"https://evil.top/" + pdf2zhAssetURL,
			"未登记的前缀"},
		{"https://ghfast.top.evil.com/" + pdf2zhAssetURL,
			"前缀被拼在别的域名前面"},
		{pdf2zhAssetURL + "?x=1", "加了查询串就不再全等"},
		{pdf2zhAssetURL + "/../etc/passwd", "路径穿越"},
		{" " + pdf2zhAssetURL, "前导空格（调用方会 Trim，但判定本身不该放行）"},
		{"https://ghfast.top/", "只有前缀"},
		{"HTTPS://GITHUB.COM/", "协议大小写变形"},
	}
	for _, d := range deny {
		if pdf2zhProbeURLAllowed(d.url) {
			t.Errorf("必须拒绝（%s）：%q", d.why, d.url)
		}
	}
}

// TestPdf2zhAssetURLIsStable 权威地址是白名单的基准，写错一处会让所有线路全废。
//
// 断言它的形状而不是逐字比对全文：标签与文件名是发布事实，改动时应当同时改
// 前端 PDF2ZH_ASSET，这条至少保证它指向本仓库的 release 下载路径。
func TestPdf2zhAssetURLIsStable(t *testing.T) {
	if !strings.HasPrefix(pdf2zhAssetURL, "https://github.com/leobba/litflow/releases/download/") {
		t.Errorf("资产地址必须指向本仓库的 release 下载路径，实际 %s", pdf2zhAssetURL)
	}
	if !strings.HasSuffix(pdf2zhAssetURL, ".zip") {
		t.Errorf("资产应当是 zip，实际 %s", pdf2zhAssetURL)
	}
	if !strings.Contains(pdf2zhAssetURL, "pdf2zh-v2.9.0") {
		t.Errorf("地址里应带 tag，实际 %s", pdf2zhAssetURL)
	}
}

// TestPdf2zhProbePrefixesAreExact 前缀必须以 `/` 结尾。
//
// 少了斜杠，`https://ghfast.top` 与 `https://ghfast.top/` 就是两个不同的前缀，
// 而后者才是拼接用的形态；写错会让白名单静默失效（全部判成非法）。
func TestPdf2zhProbePrefixesAreExact(t *testing.T) {
	for _, p := range pdf2zhProbePrefixes {
		if p == "" {
			continue // 直连
		}
		if !strings.HasSuffix(p, "/") {
			t.Errorf("加速前缀必须以 / 结尾：%q", p)
		}
		if !strings.HasPrefix(p, "https://") {
			t.Errorf("加速前缀必须是 https：%q", p)
		}
	}
}
