package datasource

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// 龙虎榜（M3a）：东财 datacenter 两张报表，走 DataCenterQuery 网关（包级令牌桶 QPS≤2）。
//   - RPT_DAILYBILLBOARD_DETAILSNEW 每日龙虎榜详情：全市场一天约 100~400 行（含可转债，
//     需过滤），同一只股票同日可因多个上榜原因出现多行，行以 CHANGE_TYPE（上榜类别代码）区分。
//   - RPT_ORGANIZATION_TRADE_DETAILS 机构买卖每日统计：每日约 30~80 行，机构专用席位的
//     买卖次数与净买额（「龙虎榜机构买入」加分项的权威来源，比解析 EXPLAIN 附注可靠）。
//
// 字段锚点（2026-07-08 实测）：上榜原因在 EXPLANATION（EXPLAIN 是「N家机构买入，成功率x%」
// 类附注，可为 null）；SECURITY_TYPE_CODE 058 前缀=股票、060=可转债；金额字段单位元；
// 机构表 FREECAP 单位是亿（与主表 FREE_MARKET_CAP 元不同，不落库避免混淆）。

const (
	lhbReportDaily = "RPT_DAILYBILLBOARD_DETAILSNEW"
	lhbReportOrg   = "RPT_ORGANIZATION_TRADE_DETAILS"
	lhbMaxPages    = 10 // 单日护栏：全天含转债 <500 行，pageSize=500 一页即全量
)

// LhbRow 龙虎榜单行（一只股票的一个上榜原因）。
type LhbRow struct {
	Symbol       string  // 6 位代码
	Name         string
	TradeDate    string  // YYYY-MM-DD
	ChangeType   string  // 上榜类别代码（同股同日多行的区分键）
	Reason       string  // 上榜原因（EXPLANATION）
	Note         string  // 东财附注（EXPLAIN，如「1家机构买入，成功率58.41%」，可空）
	Close        float64 // 收盘价
	ChangePct    float64 // 当日涨跌幅 %
	NetBuy       float64 // 龙虎榜净买额（元）
	BuyAmt       float64 // 龙虎榜买入额（元）
	SellAmt      float64 // 龙虎榜卖出额（元）
	DealAmt      float64 // 龙虎榜成交额（元）
	AccumAmount  float64 // 当日市场总成交额（元）
	NetRatio     float64 // 净买额占总成交比 %
	TurnoverRate float64 // 换手率 %
	FloatCap     float64 // 流通市值（元）
}

// LhbOrgRow 机构买卖每日统计单行。
type LhbOrgRow struct {
	Symbol    string
	Name      string
	TradeDate string
	Close     float64
	ChangePct float64
	BuyTimes  int     // 机构席位买入次数
	SellTimes int     // 机构席位卖出次数
	BuyAmt    float64 // 机构买入额（元）
	SellAmt   float64 // 机构卖出额（元）
	NetBuy    float64 // 机构净买额（元）
	NetRatio  float64 // 净买额占总成交比 %
	Reason    string  // 上榜原因
}

// lhbStockRow 该行是否 A 股股票：类型码 058 前缀（060=可转债等衍生品）+ 代码可映射 secid。
// 双保险防上游类型码漂移；B 股（900/200 前缀）cnSecid 不识别自然排除。
func lhbStockRow(typeCode, symbol string) bool {
	if !strings.HasPrefix(typeCode, "058") {
		return false
	}
	_, ok := cnSecid(symbol)
	return ok
}

// GetLhbDaily 拉取某交易日全市场龙虎榜详情（已过滤为 A 股股票行）。
// date 形如 2026-07-07。无数据（非交易日/当日未出榜）返回 ErrNoData。
func (e *EastMoneyAdapter) GetLhbDaily(ctx context.Context, date string) ([]LhbRow, error) {
	it := e.DataCenterQuery(DataCenterQuery{
		ReportName:  lhbReportDaily,
		Filter:      fmt.Sprintf("(TRADE_DATE='%s')", date),
		SortColumns: "BILLBOARD_NET_AMT",
		SortTypes:   "-1",
	})
	var out []LhbRow
	for page := 0; page < lhbMaxPages; page++ {
		raws, err := it.Next(ctx)
		if err != nil {
			return nil, err
		}
		if raws == nil {
			break
		}
		for _, raw := range raws {
			r, perr := ParseDcRow(raw)
			if perr != nil {
				continue
			}
			if row, ok := parseLhbRow(r); ok {
				out = append(out, row)
			}
		}
	}
	if len(out) == 0 {
		return nil, ErrNoData
	}
	return out, nil
}

// parseLhbRow 单行解析（抽出便于单测，防上游字段漂移）。
func parseLhbRow(r DcRow) (LhbRow, bool) {
	sym := r.String("SECURITY_CODE")
	if !lhbStockRow(r.String("SECURITY_TYPE_CODE"), sym) {
		return LhbRow{}, false
	}
	return LhbRow{
		Symbol:       sym,
		Name:         r.String("SECURITY_NAME_ABBR"),
		TradeDate:    r.Date("TRADE_DATE"),
		ChangeType:   r.String("CHANGE_TYPE"),
		Reason:       r.String("EXPLANATION"),
		Note:         r.String("EXPLAIN"),
		Close:        r.Float("CLOSE_PRICE"),
		ChangePct:    r.Float("CHANGE_RATE"),
		NetBuy:       r.Float("BILLBOARD_NET_AMT"),
		BuyAmt:       r.Float("BILLBOARD_BUY_AMT"),
		SellAmt:      r.Float("BILLBOARD_SELL_AMT"),
		DealAmt:      r.Float("BILLBOARD_DEAL_AMT"),
		AccumAmount:  r.Float("ACCUM_AMOUNT"),
		NetRatio:     r.Float("DEAL_NET_RATIO"),
		TurnoverRate: r.Float("TURNOVERRATE"),
		FloatCap:     r.Float("FREE_MARKET_CAP"),
	}, true
}

// GetLhbOrgDaily 拉取某交易日机构买卖每日统计（已过滤为可识别的 A 股代码）。
func (e *EastMoneyAdapter) GetLhbOrgDaily(ctx context.Context, date string) ([]LhbOrgRow, error) {
	it := e.DataCenterQuery(DataCenterQuery{
		ReportName:  lhbReportOrg,
		Filter:      fmt.Sprintf("(TRADE_DATE='%s')", date),
		SortColumns: "NET_BUY_AMT",
		SortTypes:   "-1",
	})
	var out []LhbOrgRow
	seen := map[string]bool{}
	for page := 0; page < lhbMaxPages; page++ {
		raws, err := it.Next(ctx)
		if err != nil {
			return nil, err
		}
		if raws == nil {
			break
		}
		for _, raw := range raws {
			r, perr := ParseDcRow(raw)
			if perr != nil {
				continue
			}
			row, ok := parseLhbOrgRow(r)
			if !ok || seen[row.Symbol] {
				continue // 机构统计按股聚合，同股理论一行；重复行保序取首行
			}
			seen[row.Symbol] = true
			out = append(out, row)
		}
	}
	if len(out) == 0 {
		return nil, ErrNoData
	}
	return out, nil
}

func parseLhbOrgRow(r DcRow) (LhbOrgRow, bool) {
	sym := r.String("SECURITY_CODE")
	if _, ok := cnSecid(sym); !ok {
		return LhbOrgRow{}, false
	}
	return LhbOrgRow{
		Symbol:    sym,
		Name:      r.String("SECURITY_NAME_ABBR"),
		TradeDate: r.Date("TRADE_DATE"),
		Close:     r.Float("CLOSE_PRICE"),
		ChangePct: r.Float("CHANGE_RATE"),
		BuyTimes:  int(r.Float("BUY_TIMES")),
		SellTimes: int(r.Float("SELL_TIMES")),
		BuyAmt:    r.Float("BUY_AMT"),
		SellAmt:   r.Float("SELL_AMT"),
		NetBuy:    r.Float("NET_BUY_AMT"),
		NetRatio:  r.Float("RATIO"),
		Reason:    r.String("EXPLANATION"),
	}, true
}

// ErrLhbNotReady 龙虎榜当日数据尚未发布（datacenter 盘后逐步落库，18:00 前可能不全）。
// 语义上与 ErrNoData 区分：调用方可选择稍后重试而非记「当日无榜」。
var ErrLhbNotReady = errors.New("龙虎榜数据尚未发布")
