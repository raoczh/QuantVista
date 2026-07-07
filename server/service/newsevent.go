package service

import (
	"encoding/json"
	"strings"
	"time"

	"quantvista/common"
	"quantvista/model"
)

// N2 收盘日报「今日重要事件」：4 步硬规则先行，LLM 只写摘要。
// ①约 40 词降噪黑名单过滤盘面播报类噪声；②三维可解释打分（来源级别/影响范围/
// 资金敏感度），总分 ≥6 保留、≥10 标重磅；③同主线合并（板块重叠或标题相似）；
// ④截断 Top 8~12。打分明细随日报快照落库（透明池风格，可复查每条为什么入选）。

const (
	eventKeepScore  = 6  // 保留阈值
	eventMajorScore = 10 // 重磅阈值
	eventTopN       = 12 // 截断上限（Top 8~12 取上限，不足自然少）
)

// reportEvent 单条事件（含打分明细，随快照落库可查）。
type reportEvent struct {
	Title    string   `json:"title"`
	Source   string   `json:"source"`
	Time     string   `json:"time"` // HH:MM
	Score    int      `json:"score"`
	SrcLevel int      `json:"src_level"`  // 来源级别：中央5/部委4/交易所3/重要电报3/一般1~2
	Impact   int      `json:"impact"`     // 影响范围：全市场5/板块3/个股1
	FundSens int      `json:"fund_sens"`  // 资金敏感度：直接5/间接3/弱1
	Major    bool     `json:"major,omitempty"`
	Sectors  []string `json:"sectors,omitempty"`
	Merged   int      `json:"merged,omitempty"` // 同主线被合并掉的条数
	Senti    string   `json:"sentiment,omitempty"`
}

// eventNoiseWords 降噪黑名单（约 40 词）：盘面播报/数据罗列/技术面复述类，
// 它们是行情的镜像而非增量信息，进事件段只会稀释真正的驱动性消息。
var eventNoiseWords = []string{
	"收评", "午评", "早盘", "尾盘", "盘前", "盘中必读", "复盘", "竞价",
	"龙虎榜", "大宗交易", "融资余额", "融券余额", "北向资金", "主力资金", "资金流向", "净流入居前",
	"涨停复盘", "连板天梯", "炸板", "涨停家数", "跌停家数", "个股异动", "异动点评",
	"快速拉升", "直线拉升", "触及涨停", "打开涨停", "打开跌停", "振幅达", "换手率达",
	"成交额突破", "获利盘", "筹码集中", "技术形态", "K线", "均线", "MACD", "缺口回补",
	"新股申购", "中签号", "上市首日", "市值蒸发", "股价创",
}

// 来源级别关键词（政策层级；policy_level 已由情绪增强判定时优先用它）。
var (
	eventCentralWords = []string{"国务院", "中共中央", "中央政治局", "国常会", "央行", "中国人民银行", "习近平", "总理"}
	eventMinistryWords = []string{"证监会", "发改委", "财政部", "工信部", "商务部", "住建部", "金融监管总局", "国家统计局", "部委", "税务总局"}
	eventExchangeWords = []string{"上交所", "深交所", "北交所", "交易所", "中金所", "中登"}
)

// 全市场影响关键词 / 资金敏感关键词。
var (
	eventMarketWideWords = []string{"A股", "股市", "大盘", "指数", "宏观", "GDP", "CPI", "PMI", "货币政策", "财政政策", "美联储", "加息", "汇率"}
	eventFundDirectWords = []string{"降准", "降息", "利率", "印花税", "IPO", "再融资", "回购", "减持", "增持", "社保基金", "养老金", "险资", "汇金", "平准基金", "流动性", "逆回购", "LPR"}
	eventFundIndirectWords = []string{"政策", "规划", "试点", "补贴", "关税", "出口管制", "方案", "意见", "条例", "准入"}
)

func containsAny(text string, words []string) bool {
	for _, w := range words {
		if strings.Contains(text, w) {
			return true
		}
	}
	return false
}

// scoreNewsEvent 三维可解释打分（纯函数，可测）。
func scoreNewsEvent(n model.News) (srcLevel, impact, fundSens int) {
	text := n.Title + " " + n.Summary

	// 来源级别：情绪增强给出的 policy_level 优先；否则关键词推断；再退源侧标记。
	switch {
	case n.PolicyLevel >= 3:
		srcLevel = n.PolicyLevel
	case containsAny(text, eventCentralWords):
		srcLevel = 5
	case containsAny(text, eventMinistryWords):
		srcLevel = 4
	case containsAny(text, eventExchangeWords):
		srcLevel = 3
	case n.ImportantMark:
		srcLevel = 3 // 财联社标记的重要电报
	case n.SourcePriority <= 1:
		srcLevel = 2
	default:
		srcLevel = 1
	}

	// 影响范围：增强的 impact_scope 优先；否则按板块/个股关联与全市场词推断。
	switch n.ImpactScope {
	case "market":
		impact = 5
	case "sector":
		impact = 3
	case "stock":
		impact = 1
	default:
		switch {
		case containsAny(text, eventMarketWideWords):
			impact = 5
		case n.RelatedSectors != "" && n.RelatedSectors != "[]":
			impact = 3
		case n.RelatedSymbols != "" && n.RelatedSymbols != "[]":
			impact = 1
		default:
			impact = 2
		}
	}

	// 资金敏感度。
	switch {
	case containsAny(text, eventFundDirectWords):
		fundSens = 5
	case containsAny(text, eventFundIndirectWords):
		fundSens = 3
	default:
		fundSens = 1
	}
	return
}

// eventSectors 解析 related_sectors JSON（坏值容错）。
func eventSectors(n model.News) []string {
	if n.RelatedSectors == "" {
		return nil
	}
	var out []string
	if json.Unmarshal([]byte(n.RelatedSectors), &out) != nil {
		return nil
	}
	return out
}

// sameMainline 同主线判定：标题相似（Dice ≥0.55）或共享至少一个板块。
func sameMainline(a, b reportEvent) bool {
	if bigramDice(normalizeNewsTitle(a.Title), normalizeNewsTitle(b.Title)) >= 0.55 {
		return true
	}
	for _, sa := range a.Sectors {
		for _, sb := range b.Sectors {
			if sa == sb && sa != "" {
				return true
			}
		}
	}
	return false
}

// buildTodayEvents 当日事件流水线（DB 读 + 纯规则）。date 为 2006-01-02。
func buildTodayEvents(date string) []reportEvent {
	if common.DB == nil {
		return nil
	}
	dayStart, err := time.ParseInLocation("2006-01-02", date, time.Local)
	if err != nil {
		return nil
	}
	var rows []model.News
	if err := common.DB.
		Select("id, title, summary, source, category, publish_time, related_symbols, related_sectors, source_priority, sentiment, impact_scope, policy_level, important_mark").
		Where("category IN ? AND publish_time >= ? AND publish_time < ?",
			[]string{"telegraph", "flash"}, dayStart, dayStart.Add(24*time.Hour)).
		Order("publish_time DESC").Limit(300).Find(&rows).Error; err != nil {
		return nil
	}
	return selectReportEvents(rows)
}

// selectReportEvents 4 步硬规则（纯函数，可测）：降噪 → 打分 → 同主线合并 → Top N。
func selectReportEvents(rows []model.News) []reportEvent {
	// ①降噪 + ②打分（≥6 保留）。
	kept := make([]reportEvent, 0, 32)
	for _, n := range rows {
		if containsAny(n.Title, eventNoiseWords) {
			continue
		}
		src, impact, fund := scoreNewsEvent(n)
		total := src + impact + fund
		if total < eventKeepScore {
			continue
		}
		kept = append(kept, reportEvent{
			Title: truncateRunes(n.Title, 120), Source: n.Source,
			Time: n.PublishTime.Format("15:04"),
			Score: total, SrcLevel: src, Impact: impact, FundSens: fund,
			Major: total >= eventMajorScore, Sectors: eventSectors(n),
			Senti: sentimentCN(n.Sentiment),
		})
	}

	// ③同主线合并：保留组内最高分代表，记合并条数。O(n²) 但 n 已被打分收窄。
	merged := make([]reportEvent, 0, len(kept))
	for _, e := range kept {
		found := false
		for i := range merged {
			if sameMainline(merged[i], e) {
				if e.Score > merged[i].Score {
					e.Merged = merged[i].Merged + 1
					merged[i] = e
				} else {
					merged[i].Merged++
				}
				found = true
				break
			}
		}
		if !found {
			merged = append(merged, e)
		}
	}

	// ④按分排序截断 Top N（稳定：同分保持时间序）。
	for i := 0; i < len(merged); i++ {
		for j := i + 1; j < len(merged); j++ {
			if merged[j].Score > merged[i].Score {
				merged[i], merged[j] = merged[j], merged[i]
			}
		}
	}
	if len(merged) > eventTopN {
		merged = merged[:eventTopN]
	}
	return merged
}
