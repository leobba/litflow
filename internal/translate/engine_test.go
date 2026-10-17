package translate

// pdf2zh 状态探测的测试。
//
// 这是**新装用户第一个会撞上的东西**，而它的失败形态是延迟的：配置写错时
// 网关照常启动、面板照常显示、队列照常接受任务，直到第一次提交才报错 ——
// 那时用户已经在看一篇论文了。所以这组断言的重点是「错就现在说」。

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestPdf2zhStatusConfiguredOK(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cfg := DefaultConfig()
	cfg.PDF2zhExe = exe
	q := NewQueue(cfg, "http://x/v1", "k")

	st := q.Pdf2zhStatus()
	if !st.OK {
		t.Fatalf("应当可用：%s", st.Error)
	}
	if st.Path != exe {
		t.Errorf("Path = %q，期望 %q", st.Path, exe)
	}
	if st.Source != "config" {
		t.Errorf("Source = %q，期望 config —— 界面据此说明路径来源", st.Source)
	}
}

// TestPdf2zhStatusConfiguredMissing 配了不存在的路径：必须报错，且 source 仍是
// config —— 否则界面会把它显示成「从 PATH 找到」，用户会去查 PATH。
func TestPdf2zhStatusConfiguredMissing(t *testing.T) {
	bad := filepath.Join(t.TempDir(), "nope", "pdf2zh_next.exe")
	cfg := DefaultConfig()
	cfg.PDF2zhExe = bad
	q := NewQueue(cfg, "http://x/v1", "k")

	st := q.Pdf2zhStatus()
	if st.OK {
		t.Fatal("不存在的路径不该判为可用")
	}
	if st.Source != "config" {
		t.Errorf("Source = %q，期望 config", st.Source)
	}
	if !strings.Contains(st.Error, "pdf2zh_exe") {
		t.Errorf("错误里应指出是哪个配置项，实际 %q", st.Error)
	}
	if st.Path != bad {
		t.Errorf("要回显用户写的那条路径，实际 %q", st.Path)
	}
}

// TestPdf2zhStatusEmptyFallsBackToPath 未配置时来源是 path。
//
// **必须把 PATH 清空才算测到**。这个测试的第一版没有清 PATH，于是它在开发机上
// 走的是「PATH 上找到了 pdf2zh_next」那条成功分支 —— 错误分支一次都没被执行。
// 变异测试把这件事揭出来了：把错误分支的 Source 改成 "config"，测试照样通过。
// 一个只在「本机恰好没装 pdf2zh」时才有效的断言，等于没有断言。
func TestPdf2zhStatusEmptyFallsBackToPath(t *testing.T) {
	t.Setenv("PATH", "") // 确定性地让 exec.LookPath 失败
	cfg := DefaultConfig()
	cfg.PDF2zhExe = ""
	q := NewQueue(cfg, "http://x/v1", "k")

	st := q.Pdf2zhStatus()
	if st.OK {
		t.Fatal("PATH 为空时不该判为可用")
	}
	if st.Source != "path" {
		t.Errorf("Source = %q，期望 path —— 界面据此告诉用户「去查 PATH」", st.Source)
	}
	if st.Error == "" {
		t.Error("判为不可用就必须给出原因")
	}
	if !strings.Contains(st.Error, "pdf2zh_exe") {
		t.Errorf("错误里应指出怎么修（设置 translate.pdf2zh_exe），实际 %q", st.Error)
	}
}

// TestPdf2zhStatusFoundOnPath PATH 上有时来源是 path 且可用。
// 与上一条合起来，两个分支都被确定性地覆盖，不依赖开发机上装没装。
func TestPdf2zhStatusFoundOnPath(t *testing.T) {
	dir := t.TempDir()
	// Windows 上 LookPath 认 .exe；其它平台上直接放同名文件。
	name := "pdf2zh_next"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	fake := filepath.Join(dir, name)
	if err := os.WriteFile(fake, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)

	cfg := DefaultConfig()
	cfg.PDF2zhExe = ""
	q := NewQueue(cfg, "http://x/v1", "k")

	st := q.Pdf2zhStatus()
	if !st.OK {
		t.Fatalf("PATH 上有 pdf2zh_next 时应判为可用：%s", st.Error)
	}
	if st.Source != "path" {
		t.Errorf("Source = %q，期望 path", st.Source)
	}
	if st.Path == "" {
		t.Error("判为可用就必须给出路径")
	}
}

// TestPdf2zhStatusIsOptional 探测失败不能让队列变得不可用 ——
// 这是界面上的「提醒」，不是「前置条件」。
func TestPdf2zhStatusIsOptional(t *testing.T) {
	cfg := DefaultConfig()
	cfg.PDF2zhExe = filepath.Join(t.TempDir(), "missing.exe")
	cfg.Enabled = true
	q := NewQueue(cfg, "http://x/v1", "k")

	if !q.Enabled() {
		t.Fatal("引擎缺失不该把队列标记为不可用")
	}
	// 提交依然要能成功入队（失败会发生在 worker 里，带着分类后的原因）
	dir := t.TempDir()
	pdf := filepath.Join(dir, "a.pdf")
	if err := os.WriteFile(pdf, []byte("%PDF"), 0o644); err != nil {
		t.Fatal(err)
	}
	job, err := q.Submit(pdf, dir, "", "", false)
	if err != nil {
		t.Fatalf("引擎缺失不该阻止入队: %v", err)
	}
	if job.State != StateQueued {
		t.Fatalf("状态 = %s，期望 queued", job.State)
	}
}

var _ = runtime.GOOS // 保留 runtime 引用：该文件在 Windows 上也跑，不在别处跑
