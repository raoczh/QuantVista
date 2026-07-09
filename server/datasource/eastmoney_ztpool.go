package datasource

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
)

// 涨停池族（M3a）：push2ex getTopic*Pool 五件套（涨停/炸板/昨日涨停/强势/次新）。
// 走 e.get（push2ex 无备用域，连续限流熔断快速失败——与 getTopicZDFenBu 同断路器域族）。
//
// 上游口径锚点（2026-07-08 实测）：
//   - sort 参数必带，缺失返回 rc=102 data=null（涨停/炸板 fbt:asc、昨日涨停 zs:desc、强势 zdp:desc）；
//   - 价格字段（p/ztp）为 ×1000 的整数，解析时 /1000；
//   - date 参数只保证「当日/最近交易日」有效：传历史日期时上游可能静默返回今日数据
//     （实测 date=昨日返回 qdate=今日），因此必须校验响应 qdate 与请求一致，不符按无数据处理
//     ——涨停池历史靠每日盘后快照自行积累，上游不可回溯；
//   - fbt/lbt/yfbt 为 HHMMSS 整数时间。
const (
	ztPoolUT       = "7eea3edcaed734bea9cbfc24409ed989" // 东财网页公共 ut（与涨跌分布同款）
	ztPoolPageSize = 320                                // 单页容量：极端情绪日涨停 300+，一页尽量覆盖
	ztPoolMaxPages = 5
)

// ZTPoolItem 涨停池条目。
type ZTPoolItem struct {
	Symbol       string  `json:"symbol"`
	Name         string  `json:"name"`
	Price        float64 `json:"price"`          // 收盘价（上游 ×1000 已还原）
	ChangePct    float64 `json:"change_pct"`     // 涨跌幅 %
	Amount       float64 `json:"amount"`         // 成交额（元）
	FloatCap     float64 `json:"float_cap"`      // 流通市值（元）
	TurnoverRate float64 `json:"turnover_rate"`  // 换手率 %
	Streak       int     `json:"streak"`         // 连板数（lbc）
	FirstSealAt  int     `json:"first_seal_at"`  // 首次封板时间 HHMMSS
	LastSealAt   int     `json:"last_seal_at"`   // 最后封板时间 HHMMSS
	SealFund     float64 `json:"seal_fund"`      // 封板资金（元）
	BreakCount   int     `json:"break_count"`    // 炸板次数（zbc）
	Industry     string  `json:"industry"`       // 行业板块（hybk）
	StatDays     int     `json:"stat_days"`      // 涨停统计：days 天
	StatCount    int     `json:"stat_count"`     // 涨停统计：ct 板（如 5天3板）
}

// ZBPoolItem 炸板池条目（曾封板但收盘未封住）。
type ZBPoolItem struct {
	Symbol       string  `json:"symbol"`
	Name         string  `json:"name"`
	Price        float64 `json:"price"`
	ChangePct    float64 `json:"change_pct"`
	TurnoverRate float64 `json:"turnover_rate"`
	BreakCount   int     `json:"break_count"` // 炸板次数
	Industry     string  `json:"industry"`
}

// YZTPoolItem 昨日涨停池条目（昨日涨停股的今日表现——情绪溢价的核心观测）。
type YZTPoolItem struct {
	Symbol       string  `json:"symbol"`
	Name         string  `json:"name"`
	ChangePct    float64 `json:"change_pct"`  // 今日涨跌幅 %（昨涨停今表现）
	YStreak      int     `json:"y_streak"`    // 昨日连板数（ylbc）
	TurnoverRate float64 `json:"turnover_rate"`
	Industry     string  `json:"industry"`
}

// ztPoolResp 池接口通用外壳。rc=102 = 无数据（非交易日/参数无效）。
type ztPoolResp struct {
	Rc   int `json:"rc"`
	Data struct {
		TC    int               `json:"tc"`    // 池内总家数
		QDate int64             `json:"qdate"` // 数据实际归属日 YYYYMMDD（必须校验）
		Pool  []json.RawMessage `json:"pool"`
	} `json:"data"`
}

// fetchZTPoolPages 拉取某池全部分页并逐行回调。date 形如 20260708。
// 返回 (池内总家数, error)；qdate 与请求 date 不符视为无该日数据（防错日落库）。
func (e *EastMoneyAdapter) fetchZTPoolPages(ctx context.Context, api, sort, date string, onRow func(json.RawMessage)) (int, error) {
	total := 0
	got := 0
	for page := 0; page < ztPoolMaxPages; page++ {
		url := fmt.Sprintf(
			"https://push2ex.eastmoney.com/%s?ut=%s&dpt=wz.ztzt&Pageindex=%d&pagesize=%d&sort=%s&date=%s",
			api, ztPoolUT, page, ztPoolPageSize, sort, date,
		)
		body, status, err := e.get(ctx, url, map[string]string{"Referer": "https://quote.eastmoney.com/"})
		if err != nil {
			return 0, fmt.Errorf("%w: %v", ErrUpstream, err)
		}
		if status != http.StatusOK {
			return 0, fmt.Errorf("%w: http %d", ErrUpstream, status)
		}
		var parsed ztPoolResp
		if err := json.Unmarshal(body, &parsed); err != nil {
			return 0, fmt.Errorf("%w: %s 解析失败 %v", ErrUpstream, api, err)
		}
		if parsed.Rc != 0 || parsed.Data.TC == 0 {
			if page == 0 {
				return 0, ErrNoData // 非交易日/该池当日为空
			}
			break
		}
		if q := strconv.FormatInt(parsed.Data.QDate, 10); q != date {
			// 上游对无效 date 会静默回落到最近交易日数据，错日落库是毒数据，拒绝。
			return 0, fmt.Errorf("%w: %s 返回 %s 数据（请求 %s，该池不可回溯历史）", ErrNoData, api, q, date)
		}
		total = parsed.Data.TC
		for _, raw := range parsed.Data.Pool {
			onRow(raw)
			got++
		}
		if got >= total || len(parsed.Data.Pool) == 0 {
			break
		}
	}
	return total, nil
}

// GetZTPool 涨停池（date=YYYYMMDD，仅当日/最近交易日有效）。
func (e *EastMoneyAdapter) GetZTPool(ctx context.Context, date string) ([]ZTPoolItem, error) {
	type row struct {
		C    string          `json:"c"`
		N    string          `json:"n"`
		P    json.RawMessage `json:"p"`
		Zdp  json.RawMessage `json:"zdp"`
		Amt  json.RawMessage `json:"amount"`
		Ltsz json.RawMessage `json:"ltsz"`
		Hs   json.RawMessage `json:"hs"`
		Lbc  json.RawMessage `json:"lbc"`
		Fbt  json.RawMessage `json:"fbt"`
		Lbt  json.RawMessage `json:"lbt"`
		Fund json.RawMessage `json:"fund"`
		Zbc  json.RawMessage `json:"zbc"`
		Hybk string          `json:"hybk"`
		Zttj struct {
			Days int `json:"days"`
			Ct   int `json:"ct"`
		} `json:"zttj"`
	}
	var out []ZTPoolItem
	_, err := e.fetchZTPoolPages(ctx, "getTopicZTPool", "fbt:asc", date, func(raw json.RawMessage) {
		var it row
		if json.Unmarshal(raw, &it) != nil || it.C == "" {
			return
		}
		p, _ := emNum(it.P)
		zdp, _ := emNum(it.Zdp)
		amt, _ := emNum(it.Amt)
		ltsz, _ := emNum(it.Ltsz)
		hs, _ := emNum(it.Hs)
		lbc, _ := emNum(it.Lbc)
		fbt, _ := emNum(it.Fbt)
		lbt, _ := emNum(it.Lbt)
		fund, _ := emNum(it.Fund)
		zbc, _ := emNum(it.Zbc)
		out = append(out, ZTPoolItem{
			Symbol: it.C, Name: it.N,
			Price: p / 1000, ChangePct: zdp, Amount: amt, FloatCap: ltsz, TurnoverRate: hs,
			Streak: int(lbc), FirstSealAt: int(fbt), LastSealAt: int(lbt),
			SealFund: fund, BreakCount: int(zbc), Industry: it.Hybk,
			StatDays: it.Zttj.Days, StatCount: it.Zttj.Ct,
		})
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// GetZBPool 炸板池。
func (e *EastMoneyAdapter) GetZBPool(ctx context.Context, date string) ([]ZBPoolItem, error) {
	type row struct {
		C    string          `json:"c"`
		N    string          `json:"n"`
		P    json.RawMessage `json:"p"`
		Zdp  json.RawMessage `json:"zdp"`
		Hs   json.RawMessage `json:"hs"`
		Zbc  json.RawMessage `json:"zbc"`
		Hybk string          `json:"hybk"`
	}
	var out []ZBPoolItem
	_, err := e.fetchZTPoolPages(ctx, "getTopicZBPool", "fbt:asc", date, func(raw json.RawMessage) {
		var it row
		if json.Unmarshal(raw, &it) != nil || it.C == "" {
			return
		}
		p, _ := emNum(it.P)
		zdp, _ := emNum(it.Zdp)
		hs, _ := emNum(it.Hs)
		zbc, _ := emNum(it.Zbc)
		out = append(out, ZBPoolItem{
			Symbol: it.C, Name: it.N, Price: p / 1000, ChangePct: zdp,
			TurnoverRate: hs, BreakCount: int(zbc), Industry: it.Hybk,
		})
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// GetYesterdayZTPool 昨日涨停池（昨日涨停股的今日表现）。
func (e *EastMoneyAdapter) GetYesterdayZTPool(ctx context.Context, date string) ([]YZTPoolItem, error) {
	type row struct {
		C    string          `json:"c"`
		N    string          `json:"n"`
		Zdp  json.RawMessage `json:"zdp"`
		Ylbc json.RawMessage `json:"ylbc"`
		Hs   json.RawMessage `json:"hs"`
		Hybk string          `json:"hybk"`
	}
	var out []YZTPoolItem
	_, err := e.fetchZTPoolPages(ctx, "getYesterdayZTPool", "zs:desc", date, func(raw json.RawMessage) {
		var it row
		if json.Unmarshal(raw, &it) != nil || it.C == "" {
			return
		}
		zdp, _ := emNum(it.Zdp)
		ylbc, _ := emNum(it.Ylbc)
		hs, _ := emNum(it.Hs)
		out = append(out, YZTPoolItem{
			Symbol: it.C, Name: it.N, ChangePct: zdp,
			YStreak: int(ylbc), TurnoverRate: hs, Industry: it.Hybk,
		})
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
