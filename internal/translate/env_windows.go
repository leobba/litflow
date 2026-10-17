//go:build windows

package translate

import (
	"os"
	"os/exec"
	"strings"
	"syscall"
)

// withNoProxy 给子进程环境补上 NO_PROXY 环回项。
//
// 这是本机实测出来的**头号坑**，而且两端都看不见原因：
//
//	ProxyEnable   = 1
//	ProxyServer   = 127.0.0.1:15057
//	ProxyOverride = localhost;127.*;...
//	NO_PROXY      = （未设置）
//
// urllib 读 ProxyOverride 因而绕过回环——所以我们的探活一直是好的；
// 而 pdf2zh 内部的 openai SDK 用 httpx，**只读 NO_PROXY 环境变量、不读注册表**。
// 于是发往 127.0.0.1:7863 的每一个请求都被交给那个代理，代理回 502。
//
// 结果：51 个连续失败任务，pdf2zh 报的是一个网关根本不发出的状态码。
// 修法只能是给子进程设环境变量——不能改用户的代理配置，那会影响正常出网。
func withNoProxy(env []string) []string {
	merged := make(map[string]string, len(env)+2)
	order := make([]string, 0, len(env)+2)
	for _, kv := range env {
		i := strings.IndexByte(kv, '=')
		if i < 0 {
			continue
		}
		k, v := kv[:i], kv[i+1:]
		if _, seen := merged[k]; !seen {
			order = append(order, k)
		}
		merged[k] = v
	}

	existing := merged["NO_PROXY"]
	if existing == "" {
		existing = merged["no_proxy"]
	}
	parts := make([]string, 0, 4)
	for _, p := range strings.Split(existing, ",") {
		if p = strings.TrimSpace(p); p != "" {
			parts = append(parts, p)
		}
	}
	for _, host := range []string{"127.0.0.1", "localhost", "::1"} {
		found := false
		for _, p := range parts {
			if p == host {
				found = true
				break
			}
		}
		if !found {
			parts = append(parts, host)
		}
	}
	value := strings.Join(parts, ",")

	set := func(k, v string) {
		if _, seen := merged[k]; !seen {
			order = append(order, k)
		}
		merged[k] = v
	}
	set("NO_PROXY", value)
	set("no_proxy", value)
	// Windows 上 Python 更可靠地读大写形式，两个都写。
	set("PYTHONUNBUFFERED", "1")

	out := make([]string, 0, len(order))
	for _, k := range order {
		out = append(out, k+"="+merged[k])
	}
	return out
}

// hideWindow 让子进程不弹黑框。
//
// 没有这个，每翻译一篇论文就会在屏幕上闪一个控制台窗口——用户对「后台服务」
// 的信任会在第一秒就没了。
func hideWindow(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.HideWindow = true
	cmd.SysProcAttr.CreationFlags |= 0x08000000 // CREATE_NO_WINDOW
}

var _ = os.Environ
