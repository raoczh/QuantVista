package datasource

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

// 个股资金流（M3a）：
//   - 排行：push2 clist 换 fid 即得（f62 今日主力净额 / f164 5日 / f174 10日），
//     一次请求覆盖沪深 A 股 top N，走 e.get（push2 断路器：限流自动降级 push2delay，
//     2026-07-08 实测本机主域被重置、备用域可用——正是断路器设计场景）；
//   - 单股逐日历史：push2his fflow/daykline（与日线同域同栈），公共 ut 硬编码，
//     lmt 控制根数（按需拉 + 缓存，消费方按日线窗口口径取 250 根，不拉全历史占库）。
const (
	fflowUT = "b2884a393a59ad64002292a3e90d46a5" // fflow 族公共 ut（社区惯例硬编码）
	// fflowRankFields：f12 代码 f14 名称 f2 价格 f3 涨跌幅 f62 今日主力净额
	// f184 主力净占比% f66 超大单 f72 大单 f78 中单 f84 小单 f164 5日主力 f174 10日主力。
	fflowRankFields = "f12,f14,f2,f3,f62,f66,f72,f78,f84,f184,f164,f174"
)

// FundFlowRankRow 资金流排行单行（金额单位元）。
type FundFlowRankRow struct {
	Symbol     string  `json:"symbol"`
	Name       string  `json:"name"`
	Price      float64 `json:"price"`
	ChangePct  float64 `json:"change_pct"`
	MainNet    float64 `json:"main_net"`     // 今日主力净额
	MainPct    float64 `json:"main_pct"`     // 今日主力净占比 %
	SuperNet   float64 `json:"super_net"`
	LargeNet   float64 `json:"large_net"`
	MediumNet  float64 `json:"medium_net"`
	SmallNet   float64 `json:"small_net"`
	MainNet5d  float64 `json:"main_net_5d"`  // 5 日主力净额
	MainNet10d float64 `json:"main_net_10d"` // 10 日主力净额
}

// fflowRankFids 排行支持的排序字段白名单。
var fflowRankFids = map[string]bool{"f62": true, "f164": true, "f174": true}

// GetFundFlowRank 全市场个股资金流排行（fid=f62/f164/f174 降序，limit ≤100）。
// fs 复用 clist 四段沪深 A 股口径（不含北交所）。
func (e *EastMoneyAdapter) GetFundFlowRank(ctx context.Context, fid string, limit int) ([]FundFlowRankRow, error) {
	if !fflowRankFids[fid] {
		return nil, fmt.Errorf("不支持的资金流排行字段 %s", fid)
	}
	if limit <= 0 || limit > 100 {
		limit = 100
	}
	url := fmt.Sprintf(
		"https://%d.push2.eastmoney.com/api/qt/clist/get?pn=1&pz=%d&po=1&np=1&fltt=2&invt=2&fid=%s&fs=%s&fields=%s",
		emNode(), limit, fid, clistSpotFS, fflowRankFields,
	)
	body, status, err := e.get(ctx, url, map[string]string{"Referer": "https://quote.eastmoney.com/"})
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUpstream, err)
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("%w: http %d", ErrUpstream, status)
	}
	return parseFundFlowRank(body)
}

// parseFundFlowRank 解析排行响应（抽出便于单测）。diff 兼容数组与对象两形态（clist 前例）。
func parseFundFlowRank(body []byte) ([]FundFlowRankRow, error) {
	var parsed struct {
		Data struct {
			Diff json.RawMessage `json:"diff"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("%w: 资金流排行解析失败 %v", ErrUpstream, err)
	}
	type emRow struct {
		F12  string          `json:"f12"`
		F14  string          `json:"f14"`
		F2   json.RawMessage `json:"f2"`
		F3   json.RawMessage `json:"f3"`
		F62  json.RawMessage `json:"f62"`
		F66  json.RawMessage `json:"f66"`
		F72  json.RawMessage `json:"f72"`
		F78  json.RawMessage `json:"f78"`
		F84  json.RawMessage `json:"f84"`
		F184 json.RawMessage `json:"f184"`
		F164 json.RawMessage `json:"f164"`
		F174 json.RawMessage `json:"f174"`
	}
	var items []emRow
	raw := parsed.Data.Diff
	if len(raw) > 0 && raw[0] == '[' {
		_ = json.Unmarshal(raw, &items)
	} else if len(raw) > 0 && raw[0] == '{' {
		m := map[string]emRow{}
		if json.Unmarshal(raw, &m) == nil {
			for i := 0; ; i++ {
				it, ok := m[strconv.Itoa(i)]
				if !ok {
					break
				}
				items = append(items, it)
			}
		}
	}
	if len(items) == 0 {
		return nil, ErrNoData
	}
	out := make([]FundFlowRankRow, 0, len(items))
	for _, it := range items {
		sym := strings.TrimSpace(it.F12)
		if sym == "" {
			continue
		}
		price, _ := emNum(it.F2)
		chg, _ := emNum(it.F3)
		mainNet, _ := emNum(it.F62)
		superNet, _ := emNum(it.F66)
		largeNet, _ := emNum(it.F72)
		mediumNet, _ := emNum(it.F78)
		smallNet, _ := emNum(it.F84)
		mainPct, _ := emNum(it.F184)
		net5, _ := emNum(it.F164)
		net10, _ := emNum(it.F174)
		out = append(out, FundFlowRankRow{
			Symbol: sym, Name: strings.TrimSpace(it.F14),
			Price: price, ChangePct: chg,
			MainNet: mainNet, MainPct: mainPct,
			SuperNet: superNet, LargeNet: largeNet, MediumNet: mediumNet, SmallNet: smallNet,
			MainNet5d: net5, MainNet10d: net10,
		})
	}
	return out, nil
}

// StockFundFlowBar 单股单日资金流（金额单位元；Close/ChangePct 为该日收盘口径，
// 上游随行返回，便于消费方免二次对齐日线）。
type StockFundFlowBar struct {
	TradeDate string  `json:"trade_date"`
	MainNet   float64 `json:"main_net"`
	SmallNet  float64 `json:"small_net"`
	MediumNet float64 `json:"medium_net"`
	LargeNet  float64 `json:"large_net"`
	SuperNet  float64 `json:"super_net"`
	MainPct   float64 `json:"main_pct"` // 主力净占比 %
	Close     float64 `json:"close"`
	ChangePct float64 `json:"change_pct"`
}

// GetStockFundFlow 单股逐日资金流历史（升序）。limit<=0 默认 250（与日线窗口对齐）。
// klines 行 15 列：date,主力,小单,中单,大单,超大单,主力%,小单%,中单%,大单%,超大单%,收盘,涨跌幅,x,x。
func (e *EastMoneyAdapter) GetStockFundFlow(ctx context.Context, market, symbol string, limit int) ([]StockFundFlowBar, error) {
	if market != "cn" {
		return nil, ErrNotSupported
	}
	secid, ok := cnSecid(symbol)
	if !ok {
		return nil, ErrSymbolInvalid
	}
	if limit <= 0 || limit > 1000 {
		limit = 250
	}
	url := fmt.Sprintf(
		"https://%d.push2his.eastmoney.com/api/qt/stock/fflow/daykline/get?secid=%s&lmt=%d&klt=101&fields1=f1,f2,f3,f7&fields2=f51,f52,f53,f54,f55,f56,f57,f58,f59,f60,f61,f62,f63,f64,f65&ut=%s",
		emNode(), secid, limit, fflowUT,
	)
	body, status, err := e.get(ctx, url, map[string]string{"Referer": "https://quote.eastmoney.com/"})
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUpstream, err)
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("%w: http %d", ErrUpstream, status)
	}
	return parseStockFundFlow(body)
}

// parseStockFundFlow 解析 fflow/daykline klines（抽出便于单测）。
func parseStockFundFlow(body []byte) ([]StockFundFlowBar, error) {
	var parsed struct {
		Data struct {
			Klines []string `json:"klines"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("%w: 资金流历史解析失败 %v", ErrUpstream, err)
	}
	if len(parsed.Data.Klines) == 0 {
		return nil, ErrNoData
	}
	atof := func(s string) float64 { v, _ := strconv.ParseFloat(strings.TrimSpace(s), 64); return v }
	out := make([]StockFundFlowBar, 0, len(parsed.Data.Klines))
	for _, line := range parsed.Data.Klines {
		parts := strings.Split(line, ",")
		if len(parts) < 6 || parts[0] == "" {
			continue
		}
		b := StockFundFlowBar{
			TradeDate: parts[0],
			MainNet:   atof(parts[1]),
			SmallNet:  atof(parts[2]),
			MediumNet: atof(parts[3]),
			LargeNet:  atof(parts[4]),
			SuperNet:  atof(parts[5]),
		}
		if len(parts) >= 7 {
			b.MainPct = atof(parts[6])
		}
		// 收盘与涨跌幅在第 12/13 列（部分历史行可能缺列，容错为 0）。
		if len(parts) >= 13 {
			b.Close = atof(parts[11])
			b.ChangePct = atof(parts[12])
		}
		out = append(out, b)
	}
	if len(out) == 0 {
		return nil, ErrNoData
	}
	return out, nil
}
