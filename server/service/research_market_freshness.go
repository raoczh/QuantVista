package service

import (
	"fmt"
	"strings"
	"time"

	"quantvista/datasource"
)

// Use the same quote/session gate as recommendations and the daily report's
// metadata-only fallback. Rejected numbers must leave the evidence value set,
// not merely acquire a prompt warning next to still-citable stale values.
func applyResearchMarketFreshness(snapshot map[string]any, judge func(time.Time) quoteFreshInfo) bool {
	expectedDate := judge(time.Time{}).ExpectedDate
	hasUsableFacts := false
	var deficiencies []string
	for _, section := range []struct{ key, label string }{
		{"indices", "指数行情"}, {"gainers", "涨幅榜"}, {"actives", "成交额榜"},
	} {
		value, exists := snapshot[section.key]
		if !exists {
			continue
		}
		rows, _ := value.([]map[string]any)
		if len(rows) == 0 {
			deficiencies = append(deficiencies, section.label+"缺失")
			continue
		}
		rejected := false
		for index, row := range rows {
			sourceTime, _ := row["source_data_time"].(string)
			observed, parseErr := time.ParseInLocation(time.DateTime, sourceTime, datasource.ChinaMarketLocation())
			freshness := judge(observed)
			if parseErr != nil {
				freshness.Status = freshStatusUnknown
				row["source_time_status"] = freshStatusUnknown
			}
			price, _ := row["price"].(float64)
			if freshness.Status == freshStatusFresh && price <= 0 {
				freshness.Status = freshStatusUnknown
			}
			row["freshness_status"] = freshness.Status
			if freshness.ExpectedDate != "" {
				row["expected_as_of"] = freshness.ExpectedDate
			}
			if freshness.Status == freshStatusFresh {
				hasUsableFacts = true
				expectedDate = freshness.ExpectedDate
				continue
			}
			rejected = true
			rows[index] = reportStaleMetadata(row, section.label+"时效或价格无法核验，数值已剥离，不得作为当前行情或当前排名依据")
		}
		if rejected {
			deficiencies = append(deficiencies, section.label+"包含过期或无法核验的数据，仅保留标识、来源与实际时点，不代表当前盘面")
		}
	}
	if expectedDate != "" {
		snapshot["expected_as_of"] = expectedDate
	}
	for _, section := range []struct{ key, label string }{
		{"breadth", "涨跌家数"}, {"fund_flow", "两市资金流"}, {"mood", "情绪温度计"},
	} {
		value, exists := snapshot[section.key]
		if !exists {
			continue
		}
		block := researchMarketDatedBlock(value)
		if block == nil {
			deficiencies = append(deficiencies, section.label+"缺失")
			continue
		}
		tradeDate, _ := block["trade_date"].(string)
		tradeDate = strings.TrimSpace(tradeDate)
		if expectedDate != "" && tradeDate == expectedDate {
			block["freshness_status"] = freshStatusFresh
			snapshot[section.key] = block
			hasUsableFacts = true
			continue
		}
		status := freshStatusStale
		if tradeDate == "" || expectedDate == "" {
			status = freshStatusUnknown
		}
		note := fmt.Sprintf("%s业务日期为 %s，当前期望日期为 %s；数值未通过时效核验，不得作为当前市场依据", section.label, orStr(tradeDate, "未知"), orStr(expectedDate, "未知"))
		block["freshness_status"] = status
		snapshot[section.key] = reportStaleMetadata(block, note)
		deficiencies = append(deficiencies, note)
	}
	if sectors, exists := snapshot["sectors"].([]map[string]any); exists {
		if len(sectors) == 0 {
			deficiencies = append(deficiencies, "板块榜单缺失")
		} else {
			// Sector providers expose no comparable source timestamp. Keep these
			// exploratory observations explicitly unknown, never stamp capture time.
			for _, sector := range sectors {
				sector["freshness_status"] = freshStatusUnknown
			}
			hasUsableFacts = true
			deficiencies = append(deficiencies, "板块榜单未提供源时间，时效为 unknown，仅供分类观察，不能确认当前领涨或用于即时操作")
		}
	}
	if len(deficiencies) > 0 {
		snapshot["data_deficiencies"] = deficiencies
		snapshot["freshness_note"] = "市场数据不完整；过期指数、榜单及错日统计的数值已剥离。其余 unknown 块仍未证实实时，必须声明数据边界"
	}
	return hasUsableFacts
}

func researchMarketDatedBlock(value any) map[string]any {
	switch block := value.(type) {
	case map[string]any:
		return block
	case *datasource.Breadth:
		if block != nil {
			return map[string]any{
				"advances": block.Advances, "declines": block.Declines, "unchanged": block.Unchanged,
				"limit_up": block.LimitUp, "limit_down": block.LimitDown,
				"trade_date": block.TradeDate, "source": block.Source, "captured_at": block.DataTime,
			}
		}
	case *datasource.MarketFundFlow:
		if block != nil {
			return map[string]any{
				"main_net": block.MainNet, "super_net": block.SuperNet, "large_net": block.LargeNet,
				"medium_net": block.MediumNet, "small_net": block.SmallNet,
				"trade_date": block.TradeDate, "source": block.Source, "captured_at": block.DataTime,
			}
		}
	}
	return nil
}
