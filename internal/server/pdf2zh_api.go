package server

// pdf2zh 安装包的可达性探测。
//
// 为什么需要一条后端接口：面板的 CSP 是 `connect-src 'self'`，浏览器根本不允许
// 跨站 fetch —— 实测控制台原文就是
//
//	Connecting to 'https://ghfast.top/https://github.com/...' violates the
//	following Content Security Policy directive: "connect-src 'self'".
//
// 于是前端那条 HEAD 探测在真浏览器里**永远走 catch 分支**，只能给一句「面板问不到
// 外部站点」的降级提示。可读，但答不出「包到底传完没有」。
//
// Go 侧不受 CSP 约束，所以由它来做 HEAD。代价是必须防 SSRF：一个「你给 URL 我就
// 去请求」的接口等于把服务器当跳板探内网。

import (
	"context"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// pdf2zhAssetURL 安装包的权威地址。
//
// 与前端 PDF2ZH_ASSET 必须一致 —— 下面的白名单校验靠这个常量比对，写错一处会让
// 所有线路都判成非法。
const pdf2zhAssetURL = "https://github.com/leobba/litflow/releases/download/" +
	"pdf2zh-v2.9.0/pdf2zh-v2.9.0-BabelDOC-v0.6.4-with-assets-win64.zip"

// pdf2zhProbePrefixes 允许探测的线路前缀。空串 = 直连。
// 这里只做「合法与否」判定，不做测速。
var pdf2zhProbePrefixes = []string{
	"",
	"https://ghfast.top/",
	"https://ghproxy.net/",
	"https://gh-proxy.com/",
}

// pdf2zhProbeCache 探测结果缓存。
//
// 打开环境检测页会触发一次 HEAD，592 MB 的资产每次都要绕一圈 CDN，切线路还会再来
// 一次。60 秒足够挡住连点，又不至于让人看到陈旧结论。
var pdf2zhProbe struct {
	mu      sync.Mutex
	at      time.Time
	url     string
	payload map[string]any
}

// pdf2zhAsset 探测安装包是否已上传完成、以及这条线路能否解析。
//
// 参数 url：要探测的完整地址。**只接受**「已知前缀 + 权威地址」的组合，其余 400。
func (h *Handler) pdf2zhAsset(w http.ResponseWriter, r *http.Request) {
	raw := strings.TrimSpace(r.URL.Query().Get("url"))
	if raw == "" {
		raw = pdf2zhAssetURL
	}
	if !pdf2zhProbeURLAllowed(raw) {
		writeErrQ(w, http.StatusBadRequest,
			"url not allowed: 只接受本项目的 pdf2zh 资产地址（可由已知加速前缀包裹）")
		return
	}

	pdf2zhProbe.mu.Lock()
	if pdf2zhProbe.payload != nil && pdf2zhProbe.url == raw &&
		time.Since(pdf2zhProbe.at) < 60*time.Second {
		cached := pdf2zhProbe.payload
		pdf2zhProbe.mu.Unlock()
		writeJSONQ(w, http.StatusOK, cached)
		return
	}
	pdf2zhProbe.mu.Unlock()

	ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, raw, nil)
	if err != nil {
		writeErrQ(w, http.StatusBadRequest, "bad url: "+err.Error())
		return
	}
	req.Header.Set("User-Agent", "litflow-pdf2zh-probe")
	req.Header.Set("Accept", "*/*")

	client := &http.Client{Timeout: 25 * time.Second}
	resp, err := client.Do(req)
	payload := map[string]any{"url": raw}
	if err != nil {
		// 网络层失败和「包没传完」必须分开说：前者让人去换线路，后者让人去等。
		payload["ok"] = false
		payload["status"] = 0
		payload["error"] = "网络层失败：" + err.Error()
		payload["hint"] = "这条线路从本机连不通，换一条试试；直连最可靠但最慢"
	} else {
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<10))
		payload["ok"] = resp.StatusCode >= 200 && resp.StatusCode < 400
		payload["status"] = resp.StatusCode
		if n, e := strconv.ParseInt(resp.Header.Get("Content-Length"), 10, 64); e == nil && n > 0 {
			payload["size"] = n
		}
		switch {
		case resp.StatusCode == http.StatusNotFound:
			payload["hint"] = "安装包还没上传完成（404），再等一会儿"
		case resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests:
			payload["hint"] = "这条线路被限流了，换一条"
		case resp.StatusCode >= 400:
			payload["hint"] = "这条线路返回了错误状态，换一条试试"
		case resp.StatusCode >= 200 && resp.StatusCode < 400:
			payload["hint"] = "安装包已就绪，可以下载"
		}
	}

	pdf2zhProbe.mu.Lock()
	pdf2zhProbe.at = time.Now()
	pdf2zhProbe.url = raw
	pdf2zhProbe.payload = payload
	pdf2zhProbe.mu.Unlock()

	writeJSONQ(w, http.StatusOK, payload)
}

// pdf2zhProbeURLAllowed 判断待探测地址是否合法。
//
// 规则：剥掉**一个**已知前缀之后，剩下的必须逐字等于权威地址。
//
// 用「剥完再全等」而不是「前缀匹配」，是因为前缀匹配会放进
// `https://ghfast.top/https://github.com/<别的仓库>` 这类变形；全等没有歧义。
// 这既挡住 SSRF（不能拿它探内网），也挡住把本接口当通用代理用。
func pdf2zhProbeURLAllowed(raw string) bool {
	for _, p := range pdf2zhProbePrefixes {
		if p == "" {
			if raw == pdf2zhAssetURL {
				return true
			}
			continue
		}
		if strings.HasPrefix(raw, p) && raw[len(p):] == pdf2zhAssetURL {
			return true
		}
	}
	return false
}
