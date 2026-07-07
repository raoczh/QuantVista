package service

import (
	"strings"
	"testing"
	"time"

	"quantvista/common"
	"quantvista/model"
)

func TestApplySentimentRules(t *testing.T) {
	cases := []struct {
		title    string
		wantSent string
		hit      bool
	}{
		{"某公司收到证监会立案调查通知", "negative", true},
		{"某公司发布业绩预增公告，净利润同比增长80%", "positive", true},
		{"央行宣布降准0.5个百分点", "positive", true},
		{"今日天气晴朗适合出行", "neutral", false}, // 未命中给中性
	}
	for _, c := range cases {
		sent, score, _, hit := applySentimentRules(c.title, "")
		if sent != c.wantSent || hit != c.hit {
			t.Errorf("applySentimentRules(%q) = (%s,%v), want (%s,%v)", c.title, sent, hit, c.wantSent, c.hit)
		}
		if score < -1 || score > 1 {
			t.Errorf("score %v 越界", score)
		}
		if !hit && score != 0 {
			t.Errorf("未命中应给 0 分, got %v", score)
		}
	}
	// 正负词同现：负面规则在前，优先风险提示。
	if sent, _, _, _ := applySentimentRules("公司业绩预增但遭证监会立案调查", ""); sent != "negative" {
		t.Errorf("正负同现应判 negative, got %s", sent)
	}
}

func TestFilterSectors(t *testing.T) {
	got := filterSectors([]string{"半导体", "不存在的幻觉板块", "半导体", " 银行 ", "白酒", "光伏", "储能", "军工", "黄金"})
	if len(got) != 5 {
		t.Fatalf("应去重+白名单过滤+截断到 5, got %v", got)
	}
	for _, s := range got {
		if !newsSectorWhitelist[s] {
			t.Errorf("板块 %s 不在白名单", s)
		}
	}
	if got[0] != "半导体" || got[1] != "银行" {
		t.Errorf("顺序/trim 不符: %v", got)
	}
}

func TestNormalizeEnhance(t *testing.T) {
	it := normalizeEnhance(newsEnhanceItem{
		Sentiment: "POSITIVE", SentimentScore: 3.5, ImpactScope: "Market",
		RelatedSectors: []string{"幻觉板块", "银行"}, PolicyLevel: 7,
	})
	if it.Sentiment != "positive" || it.SentimentScore != 1 || it.ImpactScope != "market" {
		t.Errorf("归一失败: %+v", it)
	}
	if it.PolicyLevel != 0 {
		t.Errorf("非法 policy_level 应归 0, got %d", it.PolicyLevel)
	}
	if len(it.RelatedSectors) != 1 || it.RelatedSectors[0] != "银行" {
		t.Errorf("板块白名单过滤失败: %v", it.RelatedSectors)
	}
	// 方向与分数矛盾：以方向为准、分数归 0。
	it2 := normalizeEnhance(newsEnhanceItem{Sentiment: "negative", SentimentScore: 0.8})
	if it2.SentimentScore != 0 {
		t.Errorf("方向矛盾应压 0, got %v", it2.SentimentScore)
	}
	it3 := normalizeEnhance(newsEnhanceItem{Sentiment: "看涨乱写"})
	if it3.Sentiment != "neutral" {
		t.Errorf("非法枚举应归 neutral, got %s", it3.Sentiment)
	}
}

// TestStockDailySentiment 聚合情绪分：来源权重加权、(symbol,date) 幂等一天一次。
func TestStockDailySentiment(t *testing.T) {
	setupTestDB(t)
	now := time.Now()
	date := now.Format("2006-01-02")
	mk := func(id int64, score float64, prio int, senti string) {
		common.DB.Create(&model.News{
			ID: id, Title: "t", RelatedSymbols: `["600001"]`, Source: "cls",
			PublishTime: now, CollectTime: now, ContentHash: string(rune('a'+id)) + date,
			SourcePriority: prio, Sentiment: senti, SentimentScore: score,
		})
	}
	mk(1, 0.9, 1, "positive") // 权重 3
	mk(2, -0.3, 3, "negative") // 权重 1
	mk(3, 0.5, 2, "")          // 未增强，不参与

	score, cnt, ok := stockDailySentiment("600001", date)
	if !ok || cnt != 2 {
		t.Fatalf("ok=%v cnt=%d, want true 2", ok, cnt)
	}
	want := (0.9*3 + (-0.3)*1) / 4 // = 0.6
	if score < want-0.01 || score > want+0.01 {
		t.Errorf("score = %v, want ≈%v", score, want)
	}

	// 幂等：再插新闻不改变已落库的当日聚合分（一天只算一次）。
	mk(4, -1, 1, "negative")
	score2, cnt2, _ := stockDailySentiment("600001", date)
	if score2 != score || cnt2 != cnt {
		t.Errorf("(symbol,date) 应幂等复用缓存: got (%v,%d) want (%v,%d)", score2, cnt2, score, cnt)
	}

	// 无新闻标的：落 0 分 0 条、ok=false。
	if _, cnt3, ok3 := stockDailySentiment("600999", date); ok3 || cnt3 != 0 {
		t.Errorf("无新闻应 (0,false), got cnt=%d ok=%v", cnt3, ok3)
	}
}

// TestStrategyAdjustSentiment 消息面因子：利好加分、利空扣分更重、噪声带不动分。
func TestStrategyAdjustSentiment(t *testing.T) {
	f := &candFactors{BarCount: 60}
	base := candidate{Symbol: "600001"}

	pos := base
	pos.SentiScore, pos.SentiNews = 0.72, 5
	d1, notes1 := strategyAdjust(model.RecTypeShortTerm, "momentum", pos, f)
	neg := base
	neg.SentiScore, neg.SentiNews = -0.5, 3
	d2, _ := strategyAdjust(model.RecTypeShortTerm, "momentum", neg, f)
	flat := base
	flat.SentiScore, flat.SentiNews = 0.1, 2
	d3, _ := strategyAdjust(model.RecTypeShortTerm, "momentum", flat, f)

	if d1-d3 != 3 {
		t.Errorf("利好应 +3: d1=%v d3=%v", d1, d3)
	}
	if d3-d2 != 4 {
		t.Errorf("利空应 -4: d2=%v d3=%v", d2, d3)
	}
	found := false
	for _, n := range notes1 {
		if strings.Contains(n, "利好情绪") {
			found = true
		}
	}
	if !found {
		t.Errorf("加分说明缺失: %v", notes1)
	}
	// 无新闻：不动分。
	d4, _ := strategyAdjust(model.RecTypeShortTerm, "momentum", base, f)
	if d4 != d3 {
		t.Errorf("无新闻不应动分: d4=%v d3=%v", d4, d3)
	}
	// 情绪分进证据核验值域。
	vals := candidateValueSet(pos)
	hit := false
	for _, v := range vals {
		if v == 0.72 {
			hit = true
		}
	}
	if !hit {
		t.Errorf("senti_score 未进核验值域")
	}
}

func TestFallbackMarketSignals(t *testing.T) {
	cases := []struct {
		pct  float64
		want string
	}{
		{6, "大涨(≥5%)"}, {2, "上涨(1%~5%)"}, {0.2, "平稳(±1%)"}, {-3, "下跌(-5%~-1%)"}, {-7, "大跌(≤-5%)"},
	}
	for _, c := range cases {
		out := fallbackMarketSignals(c.pct, nil, 0)
		if out["change_band"] != c.want {
			t.Errorf("change_band(%v) = %v, want %v", c.pct, out["change_band"], c.want)
		}
	}
}
