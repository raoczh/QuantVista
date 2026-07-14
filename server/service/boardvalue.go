package service

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"quantvista/common"
	"quantvista/datasource"
	"quantvista/model"

	"gorm.io/gorm/clause"
)

// P3b 板块估值聚合：每日 16:10 全市场增量成功后，对 clist 快照按 f100 行业名
// groupBy 聚合中位 PE(TTM)/PB 落 BoardValuationDaily（挂点与 RebuildFactorTableAsync
// 并排，见 marketwide.go）。范围诚实：只覆盖行业板块（个股 f100 只有行业归属，
// 概念板块无覆盖）；横截面分位当日落库，250 日时序分位查询侧按积累天数如实计算。
//
// 行业名→BK 码映射每次现拉 GetBoardList（约 5 页轻请求）：f100 与行业板块 f14 名
// 精确匹配是 2026-07-10 实测锚点（110/110），映射不上的行业跳过并打日志——
// 靠日志发现上游改名，而不是静默丢数据。板块清单拉取失败只日志，次日增量再来（best-effort）。

const boardValHistWindow = 250 // 时序分位窗口（与日线/因子窗口对齐）

// boardLister 聚合链路依赖的数据源能力子集（单测注入假实现）。
type boardLister interface {
	GetBoardList(ctx context.Context, kind string) ([]datasource.BoardListItem, error)
}

// boardValMu 聚合防抖（TryLock：手动补跑与定时增量撞车时后来者直接放弃）。
var boardValMu sync.Mutex

// AggregateBoardValuationAsync 异步聚合当日板块估值（best-effort，不阻塞增量主流程）。
func AggregateBoardValuationAsync(rows []datasource.SpotRow, tradeDate string) {
	if common.DB == nil {
		return
	}
	go func() {
		if !boardValMu.TryLock() {
			return
		}
		defer boardValMu.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if err := aggregateBoardValuation(ctx, datasource.NewEastMoneyAdapter(), rows, tradeDate); err != nil {
			common.SysWarn("板块估值聚合失败 %s（次日增量再试）: %v", tradeDate, err)
		}
	}()
}

// aggregateBoardValuation 聚合并落库（抽出便于单测注入假 lister/内存库）。
func aggregateBoardValuation(ctx context.Context, src boardLister, rows []datasource.SpotRow, tradeDate string) error {
	if tradeDate == "" || len(rows) == 0 {
		return fmt.Errorf("空快照或缺交易日")
	}
	boards, err := src.GetBoardList(ctx, "industry")
	if err != nil {
		return fmt.Errorf("行业板块清单拉取失败: %w", err)
	}
	nameToCode := make(map[string]string, len(boards))
	for _, b := range boards {
		if b.Name != "" {
			nameToCode[b.Name] = b.Code
		}
	}
	aggs := aggregateSpotByIndustry(rows)
	recs := make([]model.BoardValuationDaily, 0, len(aggs))
	skipped := make([]string, 0, 4)
	for _, a := range aggs {
		code, ok := nameToCode[a.Industry]
		if !ok {
			skipped = append(skipped, a.Industry)
			continue
		}
		recs = append(recs, model.BoardValuationDaily{
			Kind: "industry", BoardCode: code, BoardName: a.Industry, TradeDate: tradeDate,
			MedianPETTM: round2(a.MedianPETTM), MedianPB: round2(a.MedianPB),
			PosPECount: a.PosPECount, StockCount: a.StockCount,
		})
	}
	if len(recs) == 0 {
		return fmt.Errorf("无可落库板块（快照 %d 行、映射 %d 个板块）", len(rows), len(nameToCode))
	}
	fillCrossSectionRank(recs)
	if err := common.DB.Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "kind"}, {Name: "board_code"}, {Name: "trade_date"}},
		DoUpdates: clause.AssignmentColumns([]string{
			"board_name", "median_pe_ttm", "median_pb", "pos_pe_count", "stock_count", "pct_rank", "updated_at",
		}),
	}).CreateInBatches(recs, 200).Error; err != nil {
		return fmt.Errorf("落库失败: %w", err)
	}
	if len(skipped) > 0 {
		common.SysWarn("板块估值聚合 %s：%d 个行业名未匹配到板块码被跳过 %v（上游改名？）", tradeDate, len(skipped), skipped)
	}
	common.SysLog("板块估值聚合完成 %s：落库 %d 个行业板块（快照 %d 行）", tradeDate, len(recs), len(rows))
	return nil
}

// ---------- 纯函数（单测锚点） ----------

// boardValAgg 单行业聚合结果。
type boardValAgg struct {
	Industry    string
	MedianPETTM float64
	MedianPB    float64
	PosPECount  int
	StockCount  int
}

// aggregateSpotByIndustry 按 SpotRow.Industry 分组聚合估值中位数（按行业名排序稳定输出）。
// 只吃正样本：PE 亏损为负、缺失/停牌为 0，一律不进中位数（PosPECount 是 PE 的分母，
// PB 分母可能不同但不单独披露——中位数本就是抗噪统计）；Industry 为空（退市壳票）整行跳过。
func aggregateSpotByIndustry(rows []datasource.SpotRow) []boardValAgg {
	type bucket struct {
		pes, pbs []float64
		count    int
	}
	byInd := map[string]*bucket{}
	for _, r := range rows {
		if r.Industry == "" {
			continue
		}
		b := byInd[r.Industry]
		if b == nil {
			b = &bucket{}
			byInd[r.Industry] = b
		}
		b.count++
		if r.PETTM > 0 {
			b.pes = append(b.pes, r.PETTM)
		}
		if r.PB > 0 {
			b.pbs = append(b.pbs, r.PB)
		}
	}
	names := make([]string, 0, len(byInd))
	for name := range byInd {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]boardValAgg, 0, len(names))
	for _, name := range names {
		b := byInd[name]
		sort.Float64s(b.pes)
		sort.Float64s(b.pbs)
		out = append(out, boardValAgg{
			Industry:    name,
			MedianPETTM: median(b.pes),
			MedianPB:    median(b.pbs),
			PosPECount:  len(b.pes),
			StockCount:  b.count,
		})
	}
	return out
}

// percentileRank v 在 all 中的百分位（0~100，越高越贵）。
// 采用对称定义 100*(小于数+0.5*相等数)/n：单样本得 50（不武断说「最便宜/最贵」）。
func percentileRank(all []float64, v float64) float64 {
	if len(all) == 0 {
		return 0
	}
	less, equal := 0, 0
	for _, x := range all {
		switch {
		case x < v:
			less++
		case x == v:
			equal++
		}
	}
	return round2(100 * (float64(less) + 0.5*float64(equal)) / float64(len(all)))
}

// fillCrossSectionRank 原地填当日横截面分位：中位 TTM PE 在全部有效板块（PosPECount>0）
// 中的百分位；无有效 PE 样本的板块记 -1（区分「最便宜」与「算不出」）。
func fillCrossSectionRank(recs []model.BoardValuationDaily) {
	valid := make([]float64, 0, len(recs))
	for _, r := range recs {
		if r.PosPECount > 0 && r.MedianPETTM > 0 {
			valid = append(valid, r.MedianPETTM)
		}
	}
	for i := range recs {
		if recs[i].PosPECount > 0 && recs[i].MedianPETTM > 0 {
			recs[i].PctRank = percentileRank(valid, recs[i].MedianPETTM)
		} else {
			recs[i].PctRank = -1
		}
	}
}

// ---------- 查询侧（详情页/板块 AI 分析共用） ----------

// BoardValuationView 板块估值视图：聚合表最新行 + 查询侧算的时序分位与积累天数。
type BoardValuationView struct {
	TradeDate   string  `json:"trade_date"`
	BoardName   string  `json:"board_name"`
	MedianPETTM float64 `json:"median_pe_ttm"`
	MedianPB    float64 `json:"median_pb"`
	PosPECount  int     `json:"pos_pe_count"`
	StockCount  int     `json:"stock_count"`
	PctRank     float64 `json:"pct_rank"` // 横截面分位（当日全行业，0~100 越高越贵；-1=算不出）
	// 时序分位：当前中位 PE 在自身近 ≤250 日历史中的百分位。HistDays 是分母——
	// 积累天数少时分位无代表性，UI/prompt 必须带天数声明（诚实原则）。
	HistPctRank float64 `json:"hist_pct_rank"`
	HistDays    int     `json:"hist_days"`
}

// boardValuationByName 按板块名精确匹配估值视图（板块 AI 分析的 focus 名→BK 码入口）。
// 未匹配返回 (nil, "")——focus 是自由文本，匹配不上是常态不是错误。
func boardValuationByName(name string) (*BoardValuationView, string) {
	if common.DB == nil || name == "" {
		return nil, ""
	}
	var row model.BoardValuationDaily
	if err := common.DB.Where("board_name = ?", name).
		Order("trade_date DESC").First(&row).Error; err != nil {
		return nil, ""
	}
	return boardValuationFor(row.BoardCode), row.BoardCode
}

// boardValuationFor 查某板块估值视图（最新行 + 时序分位）。无数据返回 nil
//（概念板块/聚合未跑过），调用方按缺席处理不算错误。
func boardValuationFor(code string) *BoardValuationView {
	if common.DB == nil {
		return nil
	}
	var hist []model.BoardValuationDaily
	if err := common.DB.Where("board_code = ?", code).
		Order("trade_date DESC").Limit(boardValHistWindow).Find(&hist).Error; err != nil || len(hist) == 0 {
		return nil
	}
	latest := hist[0]
	view := &BoardValuationView{
		TradeDate: latest.TradeDate, BoardName: latest.BoardName,
		MedianPETTM: latest.MedianPETTM, MedianPB: latest.MedianPB,
		PosPECount: latest.PosPECount, StockCount: latest.StockCount,
		PctRank: latest.PctRank, HistDays: len(hist), HistPctRank: -1,
	}
	if latest.MedianPETTM > 0 {
		vals := make([]float64, 0, len(hist))
		for _, h := range hist {
			if h.MedianPETTM > 0 {
				vals = append(vals, h.MedianPETTM)
			}
		}
		if len(vals) > 0 {
			view.HistPctRank = percentileRank(vals, latest.MedianPETTM)
		}
	}
	return view
}
