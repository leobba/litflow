package panel

// 本地模型策略：关掉不想看到的模型、设一个默认翻译模型。
//
// 两件事放在一起，是因为它们解决的是同一个问题：上游给的模型目录太长（实测 20+ 个），
// 其中大多数用户永远不会选，而下拉框里挤满选项会让人选错 —— 选错的代价是这篇论文
// 按那个模型的倍率扣积分，而且是**事后才知道**。
//
// 存法：只存「关掉了哪些」和「默认是哪个」，不存整个目录。目录来自上游、会增删；
// 用户的选择是本地决定。两者分开存，上游新增模型时本地策略不受影响。

import (
	"encoding/json"
	"io"
	"net/http"
	"sort"
	"strings"
)

// patchConfig 读回当前配置、改一处、再存回去。
//
// 走 LoadConfig/SaveConfig 这条既有通路而不是自己写文件：那条通路里已经有校验与
// 热生效（改完立刻应用），自己写会绕过它们，出现「磁盘上改了但运行期没改」。
func (p *Panel) patchConfig(fn func(doc map[string]any) error) error {
	if p.cfg.LoadConfig == nil || p.cfg.SaveConfig == nil {
		return errConfigUnavailable
	}
	cur, err := p.cfg.LoadConfig()
	if err != nil {
		return err
	}
	raw, err := json.Marshal(cur)
	if err != nil {
		return err
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return err
	}
	if err := fn(doc); err != nil {
		return err
	}
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	_, err = p.cfg.SaveConfig(out)
	return err
}

type configUnavailable struct{}

func (configUnavailable) Error() string { return "config api not available" }

var errConfigUnavailable = configUnavailable{}

// markDisabled 给一条模型条目打上本地关闭标记。
//
// 用 map 而不是改 panelModelEntry 的返回类型：那个函数的调用点不止一处，改签名会把
// 「模型目录」和「本地策略」两件事耦合进去 —— 而目录来自上游、策略来自本地，它们的
// 变化原因完全不同。
func markDisabled(entry map[string]any, disabled map[string]bool) map[string]any {
	if id, _ := entry["id"].(string); id != "" && disabled[id] {
		entry["disabled"] = true
	} else {
		entry["disabled"] = false
	}
	return entry
}

// disabledModels 当前被关掉的模型集合。
func (p *Panel) disabledModels() map[string]bool {
	out := map[string]bool{}
	if p.cfg.LoadConfig == nil {
		return out
	}
	cur, err := p.cfg.LoadConfig()
	if err != nil {
		return out
	}
	raw, _ := json.Marshal(cur)
	var doc struct {
		Models struct {
			Disabled []string `json:"disabled"`
		} `json:"models"`
	}
	if json.Unmarshal(raw, &doc) != nil {
		return out
	}
	for _, id := range doc.Models.Disabled {
		out[id] = true
	}
	return out
}

// modelsPatch 处理 POST /panel/api/models/toggle。
//
//	{"id": "cn:xxx", "disabled": true}
//
// 也支持批量 {"ids": [...], "disabled": true}，因为「一次关掉一批」是这里的常见动作
// —— 二十个模型逐个点关闭比一次勾选多个慢得多。
func (p *Panel) modelsToggle(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "read body: "+err.Error())
		return
	}
	var body struct {
		ID       string   `json:"id"`
		IDs      []string `json:"ids"`
		Disabled bool     `json:"disabled"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	ids := body.IDs
	if body.ID != "" {
		ids = append(ids, body.ID)
	}
	if len(ids) == 0 {
		writeErr(w, http.StatusBadRequest, "id or ids is required")
		return
	}
	err = p.patchConfig(func(doc map[string]any) error {
		models, _ := doc["models"].(map[string]any)
		if models == nil {
			models = map[string]any{}
			doc["models"] = models
		}
		var cur []string
		if arr, ok := models["disabled"].([]any); ok {
			for _, v := range arr {
				if s, ok := v.(string); ok {
					cur = append(cur, s)
				}
			}
		}
		set := map[string]bool{}
		for _, s := range cur {
			set[s] = true
		}
		for _, id := range ids {
			if body.Disabled {
				set[id] = true
			} else {
				delete(set, id)
			}
		}
		out := make([]string, 0, len(set))
		for s := range set {
			out = append(out, s)
		}
		sort.Strings(out)
		models["disabled"] = out
		return nil
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       true,
		"ids":      ids,
		"disabled": body.Disabled,
	})
}

// translateDefaultModel 处理 POST /panel/api/translate/default-model。
//
// 为什么这个比看起来重要：Zotero 插件提交时**只发 source，不带模型**，所以这条默认值
// 决定了插件实际用哪个模型。实测：用户在面板选了 hy3，而请求日志里全是 auto ——
// 因为默认值是硬编码的 "auto"，而不是用户的选择。
func (p *Panel) translateDefaultModel(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "read body: "+err.Error())
		return
	}
	var body struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	model := strings.TrimSpace(body.Model)
	err = p.patchConfig(func(doc map[string]any) error {
		tr, _ := doc["translate"].(map[string]any)
		if tr == nil {
			tr = map[string]any{}
			doc["translate"] = tr
		}
		tr["default_model"] = model
		return nil
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	// 立刻生效，不等重启：用户改完默认模型马上翻译，得到的必须是新模型。
	if p.cfg.Translate != nil {
		p.cfg.Translate.SetDefaultModel(model)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "default_model": model})
}
