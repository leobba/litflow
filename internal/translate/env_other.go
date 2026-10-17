//go:build !windows

package translate

import (
	"os"
	"os/exec"
	"strings"
)

// withNoProxy 给子进程环境补上 NO_PROXY 环回项（非 Windows 版）。
//
// Linux/macOS 通常没有那个 WinINET 代理，但这个变量本身是有意义的：
// 用户可能设了 HTTP(S)_PROXY，而发往 127.0.0.1 的请求永远不该走它。
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
	out := make([]string, 0, len(order))
	for _, k := range order {
		out = append(out, k+"="+merged[k])
	}
	return out
}

// hideWindow 在非 Windows 上没有窗口可隐藏。
func hideWindow(cmd *exec.Cmd) {}

var _ = os.Environ
