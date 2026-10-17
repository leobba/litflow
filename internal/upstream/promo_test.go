package upstream

// modelPromotions 的测试。
//
// 这块逻辑的用户可见后果很具体：hy4-preview 有一条「夜间免费」（23:00-08:00，
// 到 11-01）。白天它不生效，如果只记生效的优惠，面板那一格就只剩 x0.29，
// 用户永远不知道它夜里免费 —— 也就不会用它。
//
// 实测数据（2026-10-17 /v3/config 的 8 条 modelPromotions）：
//
//	hy3             限时免费  factor=0  replace  validUntil=11-01  无 daily（全天生效）
//	hy4-preview     夜间免费  factor=0  replace  validUntil=11-01  daily 23:00-08:00
//	step-5-preview  独家限免  factor=0  replace  validUntil=10-25  无 daily
//	space-bunny     限时折扣  无 discount        validUntil=10-08（已过期）
//	glm-5.2         夜间折扣  factor=0.5 strikethrough 23:00-7:50 + 一条 badge-only 7:50-23:00
//	deepseek 系      错峰使用  无 discount        00:00-23:59
//
// 同一份文档里，hy3 的行内 credits 是 "x0.00"（像被折扣改写），step-5-preview
// 是 "x0.43"（像牌价）—— 上游自己不一致，这就是 RateUnverified 存在的理由。

import (
	"testing"
	"time"
)

func promoDoc() []v3ModelPromotion {
	mkPromo := func(enabled bool, prio int, ids []string, label string, factor *float64,
		credits string, daily [][2]string, until string) v3ModelPromotion {
		p := v3ModelPromotion{Enabled: enabled, Priority: prio, ModelIDs: ids}
		if label != "" {
			p.Badge = &struct {
				Label string `json:"label"`
			}{label}
		}
		if factor != nil {
			p.Discount = &struct {
				DiscountedCredits string  `json:"discountedCredits"`
				Factor            float64 `json:"factor"`
			}{credits, *factor}
		}
		p.Hover = &struct {
			TextZh string `json:"textZh"`
		}{"说明 " + label}
		sc := &struct {
			Daily []struct {
				Start string `json:"start"`
				End   string `json:"end"`
			} `json:"daily"`
			Timezone   string `json:"timezone"`
			ValidFrom  string `json:"validFrom"`
			ValidUntil string `json:"validUntil"`
		}{Timezone: "Asia/Shanghai", ValidUntil: until}
		for _, w := range daily {
			var d struct {
				Start string `json:"start"`
				End   string `json:"end"`
			}
			d.Start, d.End = w[0], w[1]
			sc.Daily = append(sc.Daily, d)
		}
		p.Schedule = sc
		return p
	}
	zero := 0.0
	// 用远期截止日，让「时段外」与「已过期」在测试里稳定可区分。
	future := "2099-01-01T00:00:00+08:00"
	past := "2020-01-01T00:00:00+08:00"
	// 一条全天生效的 daily（00:00-23:59 现在必然命中）。
	allDay := [][2]string{{"00:00", "23:59"}}
	// 一条 3 小时前的窗口，保证当前一定不在窗口内。
	now := time.Now().In(promoZone)
	pastStart := now.Add(-5 * time.Hour).Format("15:04")
	pastEnd := now.Add(-4 * time.Hour).Format("15:04")

	return []v3ModelPromotion{
		mkPromo(true, 200, []string{"free-all-day"}, "限时免费", &zero, "0x", allDay, future),
		mkPromo(true, 100, []string{"night-free"}, "夜间免费", &zero, "0x",
			[][2]string{{pastStart, pastEnd}}, future),
		mkPromo(true, 100, []string{"gone"}, "限时折扣", nil, "", nil, past),
		mkPromo(true, 50, []string{"badge-only"}, "错峰使用", nil, "", allDay, ""),
		mkPromo(false, 999, []string{"free-all-day"}, "已停用", &zero, "0x", allDay, future),
	}
}

func modelsForPromos() map[string]ModelInfo {
	m := map[string]ModelInfo{}
	for _, id := range []string{"free-all-day", "night-free", "gone", "badge-only"} {
		m[id] = ModelInfo{ID: id, Credits: "x0.79"}
	}
	return m
}

// TestApplyPromotionsActiveIsUnchanged 当前生效的优惠仍走原路径（界面显示生效价）。
func TestApplyPromotionsActiveIsUnchanged(t *testing.T) {
	out := modelsForPromos()
	applyModelPromotions(out, promoDoc())

	mi := out["free-all-day"]
	if mi.PromoFactor == nil || *mi.PromoFactor != 0 {
		t.Fatalf("全天生效的限时免费应当挂上 factor=0，实际 %v", mi.PromoFactor)
	}
	if mi.PromoCredits != "0x" {
		t.Errorf("PromoCredits = %q，期望 0x", mi.PromoCredits)
	}
	if mi.PromoLabel != "限时免费" {
		t.Errorf("PromoLabel = %q", mi.PromoLabel)
	}
	if mi.RateUnverified {
		t.Error("有生效优惠时倍率不该被标为可疑 —— 界面会显示生效价")
	}
}

// TestDisabledPromotionIsNotDeclared 上游 disabled 的优惠不算声明。
// 把它算进来会让界面宣传一段上游已经关掉的优惠。
func TestDisabledPromotionIsNotDeclared(t *testing.T) {
	out := modelsForPromos()
	applyModelPromotions(out, promoDoc())
	for _, d := range out["free-all-day"].PromoDecls {
		if d.Label == "已停用" {
			t.Fatal("disabled 的优惠不该出现在声明列表里")
		}
	}
}

// TestInactivePromotionIsDeclared 时段外的优惠必须被记下来 —— 这是本次改动的核心。
//
// 不记的后果：hy4-preview 白天只剩一个 x0.29，用户不知道它 23:00 起免费。
func TestInactivePromotionIsDeclared(t *testing.T) {
	out := modelsForPromos()
	applyModelPromotions(out, promoDoc())

	mi := out["night-free"]
	if mi.PromoLabel != "" {
		t.Error("时段外的优惠不该当生效优惠挂（否则界面会显示一个现在不存在的折扣价）")
	}
	if len(mi.PromoDecls) == 0 {
		t.Fatal("时段外的优惠必须进 PromoDecls —— 否则用户无从知道它夜里免费")
	}
	d := mi.PromoDecls[0]
	if d.Label != "夜间免费" {
		t.Errorf("Label = %q", d.Label)
	}
	if d.Active {
		t.Error("当前不在时段内，Active 应为 false")
	}
	if d.Expired {
		t.Error("截止日在未来，不该判为已过期")
	}
	if d.Window == "" {
		t.Error("时段必须带上（界面要显示「23:00-08:00」）")
	}
	if d.Factor == nil || *d.Factor != 0 {
		t.Errorf("factor 要带出来供界面判断「到点免费」，实际 %v", d.Factor)
	}
}

// TestExpiredPromotionIsDistinguishedFromOutOfWindow 「已过期」和「时段外」必须能分开。
// 前者是「这段优惠没了」，后者是「现在没有，到点会有」—— 说成同一句话会误导。
func TestExpiredPromotionIsDistinguishedFromOutOfWindow(t *testing.T) {
	out := modelsForPromos()
	applyModelPromotions(out, promoDoc())

	gone := out["gone"].PromoDecls
	if len(gone) == 0 {
		t.Fatal("已过期的优惠也要记下来，界面才能说「已结束」")
	}
	if !gone[0].Expired {
		t.Error("截止日已过，Expired 应为 true")
	}
	if gone[0].Active {
		t.Error("已过期不可能同时生效")
	}

	night := out["night-free"].PromoDecls
	if len(night) > 0 && night[0].Expired {
		t.Error("时段外≠已过期：截止日在未来时不能判 Expired")
	}
}

// TestRateUnverifiedOnlyWhenPromotionDerived 倍率可信度的判定。
//
// 这是从 corrinehu/dsh-workbuddy-connect 的 rateUnknown 借来的，判据是「这一行的
// 倍率像是折扣派生的」：有带 factor≠1 的优惠声明、但当前一条都不生效。
//
// 不能无条件打标：绝大多数模型从来没参与过优惠，把它们全标成「可疑」等于
// 让这个标记失去意义。所以 badge-only（无 factor，折扣数字只在 hover 文案里）
// 不算派生 —— 那种优惠从来不改写 credits。
func TestRateUnverifiedOnlyWhenPromotionDerived(t *testing.T) {
	out := modelsForPromos()
	applyModelPromotions(out, promoDoc())

	if !out["night-free"].RateUnverified {
		t.Error("有 factor=0 的优惠声明但当前不生效 → 倍率应标为可疑")
	}
	if out["gone"].RateUnverified {
		t.Error("没有 factor 的优惠（折扣只在文案里）不该把倍率标为可疑 —— 它从不改写 credits")
	}
	if out["badge-only"].RateUnverified {
		t.Error("生效的错峰类更不该被标为可疑")
	}
	if out["free-all-day"].RateUnverified {
		t.Error("有生效优惠的不该被标为可疑")
	}
}

// TestPromoWindowText 时段文本：无 daily = 全天（空串），多条用「、」连接。
func TestPromoWindowText(t *testing.T) {
	zero := 0.0
	_ = zero
	p := v3ModelPromotion{}
	if got := promoWindowText(&p); got != "" {
		t.Errorf("无 schedule 应为空串，实际 %q", got)
	}
	p2 := v3ModelPromotion{Schedule: &struct {
		Daily []struct {
			Start string `json:"start"`
			End   string `json:"end"`
		} `json:"daily"`
		Timezone   string `json:"timezone"`
		ValidFrom  string `json:"validFrom"`
		ValidUntil string `json:"validUntil"`
	}{}}
	if got := promoWindowText(&p2); got != "" {
		t.Errorf("无 daily 应为空串（=全天），实际 %q", got)
	}
}
