package server

// 网关下发的更新清单必须描述**它自己会发出去的那个 xpi**。
//
// 这里出过一个具体的漂移：清单原本优先读发行目录里的 update.json —— 那份文件是
// 给 GitHub Release 用的，update_link 指向
// releases/download/<tag>/dualsync@zotero.local.xpi。于是网关下发的清单和网关下发的
// 包是两个不同产物。实测：本地升到 1.2.0、清单也写着 1.2.0，而 GitHub 那个 tag 下
// 的 xpi 还是 1.1.1 —— Zotero 下完发现版本没变，会一直重试。
//
// 现在的规则：版本从本地 xpi 的 manifest 里读，链接指回本网关。这样「说的」和
// 「给的」永远来自同一个文件。

import (
	"archive/zip"
	"os"
	"path/filepath"
	"testing"
)

// writeTestXPI 造一个最小可用的 xpi（zpi = zip，含 manifest.json）。
func writeTestXPI(t *testing.T, dir, version string) string {
	t.Helper()
	p := filepath.Join(dir, pluginFileName)
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	w, err := zw.Create("manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	body := `{"version":"` + version + `","applications":{"zotero":{"id":"` + pluginID + `"}}}`
	if _, err := w.Write([]byte(body)); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestPluginVersionFromXPI(t *testing.T) {
	dir := t.TempDir()
	p := writeTestXPI(t, dir, "9.8.7")
	if got := pluginVersionFromXPI(p); got != "9.8.7" {
		t.Errorf("pluginVersionFromXPI = %q，期望 9.8.7（版本必须来自包本身）", got)
	}
}

// TestPluginVersionFromXPIMissing 拿不到版本时必须返回空串，让调用方走
// 「不编造版本」的兜底 —— 绝不回一个写死的字符串去和真包对不上。
func TestPluginVersionFromXPIMissing(t *testing.T) {
	dir := t.TempDir()

	// 不存在的文件
	if got := pluginVersionFromXPI(filepath.Join(dir, "nope.xpi")); got != "" {
		t.Errorf("文件不存在应返回空串，实际 %q", got)
	}

	// 不是 zip
	bad := filepath.Join(dir, "bad.xpi")
	os.WriteFile(bad, []byte("not a zip"), 0o644)
	if got := pluginVersionFromXPI(bad); got != "" {
		t.Errorf("非 zip 应返回空串，实际 %q", got)
	}

	// zip 里没有 manifest.json
	noManifest := filepath.Join(dir, "nomanifest.xpi")
	f, _ := os.Create(noManifest)
	zw := zip.NewWriter(f)
	w, _ := zw.Create("other.txt")
	w.Write([]byte("x"))
	zw.Close()
	f.Close()
	if got := pluginVersionFromXPI(noManifest); got != "" {
		t.Errorf("没有 manifest.json 应返回空串，实际 %q", got)
	}

	// manifest 里的 version 不是字符串
	weird := filepath.Join(dir, "weird.xpi")
	f2, _ := os.Create(weird)
	zw2 := zip.NewWriter(f2)
	w2, _ := zw2.Create("manifest.json")
	w2.Write([]byte(`{"version": 123}`))
	zw2.Close()
	f2.Close()
	if got := pluginVersionFromXPI(weird); got != "" {
		t.Errorf("版本类型不对应返回空串，实际 %q", got)
	}
}
