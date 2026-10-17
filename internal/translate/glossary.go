package translate

// 用户术语表。
//
// 为什么这个和 --auto-extract-glossary 是两件事：
//
//	自动抽取 = 第二遍全文档 LLM 调用，请求量翻倍。实测在按会话限流的池子上
//	          直接是 429 风暴（51 个连续失败任务），所以默认关掉。
//	用户术语表 = 一个静态 CSV，**零额外请求**，pdf2zh 只在翻译时查表。
//
// 于是「术语一致性」这个质量杠杆在不付那笔代价的前提下也能用。对这个项目的
// 使用者（反复翻译同一领域论文，术语一错全篇都错）这是最划算的一档。
//
// 文件格式（由 babeldoc/glossary.py 定义）：
//
//	source,target,tgt_lng
//	nanocapsule,纳米胶囊
//	tannic acid,单宁酸,zh-CN
//
// 第一行是表头，tgt_lng 可省略。文件名就是术语表的名字。

import (
	"encoding/csv"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// maxGlossaryFileBytes 单个术语表的大小上限。
//
// 纯防御：csv 解析是流式的，但一张几 MB 的表意味着几万个词条，而 babeldoc 会
// 把它们全量编进匹配器 —— 那不是本功能的使用规模，更像配错了路径。
const maxGlossaryFileBytes = 8 << 20

// GlossaryCheck 一份术语表的体检结果。
type GlossaryCheck struct {
	Path  string `json:"path"`
	OK    bool   `json:"ok"`
	Terms int    `json:"terms"`
	Error string `json:"error,omitempty"`
}

// CheckGlossaries 校验术语表文件，返回逐项结果。
//
// 启动时就校验，而不是等 pdf2zh 报错：一个拼错的路径会让 pdf2zh 在流水线深处
// 抛出，那时用户看到的是一篇没译的论文加一段陌生堆栈，而不是「术语表路径写错了」。
func CheckGlossaries(paths []string) []GlossaryCheck {
	out := make([]GlossaryCheck, 0, len(paths))
	for _, p := range paths {
		out = append(out, checkOneGlossary(p))
	}
	return out
}

func checkOneGlossary(p string) GlossaryCheck {
	res := GlossaryCheck{Path: p}
	st, err := os.Stat(p)
	if err != nil {
		res.Error = "文件不存在"
		return res
	}
	if st.IsDir() {
		res.Error = "这是一个目录，不是 CSV 文件"
		return res
	}
	if st.Size() > maxGlossaryFileBytes {
		res.Error = fmt.Sprintf("文件过大（%.1f MB），术语表应远小于此", float64(st.Size())/(1<<20))
		return res
	}
	if !strings.EqualFold(filepath.Ext(p), ".csv") {
		// babeldoc 的 Globe 只按 CSV 解析；扩展名不对通常意味着选错了文件。
		res.Error = "扩展名不是 .csv —— babeldoc 只解析 CSV 术语表"
		return res
	}

	f, err := os.Open(p)
	if err != nil {
		res.Error = err.Error()
		return res
	}
	defer f.Close()

	r := csv.NewReader(f)
	r.FieldsPerRecord = -1 // 容忍 tgt_lng 缺失的行
	rows, err := r.ReadAll()
	if err != nil {
		res.Error = "CSV 解析失败: " + err.Error()
		return res
	}
	if len(rows) == 0 {
		res.Error = "文件是空的"
		return res
	}

	header := make([]string, len(rows[0]))
	for i, h := range rows[0] {
		header[i] = strings.ToLower(strings.TrimSpace(h))
	}
	has := func(name string) bool {
		for _, h := range header {
			if h == name {
				return true
			}
		}
		return false
	}
	if !has("source") || !has("target") {
		res.Error = fmt.Sprintf("表头必须含 source 和 target 两列，实际是 %v", rows[0])
		return res
	}

	// 跳过表头，数有效行（source 与 target 都非空）
	terms := 0
	for _, row := range rows[1:] {
		if len(row) < 2 {
			continue
		}
		if strings.TrimSpace(row[0]) == "" || strings.TrimSpace(row[1]) == "" {
			continue
		}
		terms++
	}
	if terms == 0 {
		res.Error = "表里没有有效词条（source 与 target 都为空）"
		return res
	}
	res.OK = true
	res.Terms = terms
	return res
}

// glossaryArg 把术语表路径拼成 pdf2zh 要的单个字符串。
//
// 由 pdf2zh_next/high_level.py 的 _get_glossaries 决定：它做
// `settings.translation.glossaries.split(",")`，所以多个文件用逗号连接。
// 逗号是**分隔符**，因此包含逗号的路径无法表达 —— 这里显式拒绝并说清楚，
// 而不是让 pdf2zh 把一个路径切成两半后报"文件不存在"。
func glossaryArg(paths []string) (string, error) {
	if len(paths) == 0 {
		return "", nil
	}
	for _, p := range paths {
		if strings.Contains(p, ",") {
			return "", fmt.Errorf("术语表路径里含逗号，而逗号是 pdf2zh 的分隔符：%s（请改名或移到别的目录）", p)
		}
	}
	return strings.Join(paths, ","), nil
}
