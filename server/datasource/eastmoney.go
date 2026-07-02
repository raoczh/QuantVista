package datasource

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// EastMoneyAdapter 东方财富公开接口（push2 / push2his）。
// 免费、无需 key、覆盖最全。注意：非官方接口，字段可能变动，仅个人自用。
type EastMoneyAdapter struct{}

func NewEastMoneyAdapter() *EastMoneyAdapter { return &EastMoneyAdapter{} }

func (e *EastMoneyAdapter) Name() string { return "eastmoney" }

// 东财行情字段（部分需 /100 还原）：
// f43 现价, f44 最高, f45 最低, f46 今开, f47 成交量(手), f48 成交额,
// f58 名称, f60 昨收, f86 行情时间(unix 秒), f170 涨跌幅(%), f169 涨跌额。
type emQuoteResp struct {
	Data map[string]json.RawMessage `json:"data"`
}

// emNum 解析东财数值字段：可能是数字或字符串 "-"（停牌/无值）。
func emNum(raw json.RawMessage) (float64, bool) {
	if len(raw) == 0 {
		return 0, false
	}
	var f float64
	if err := json.Unmarshal(raw, &f); err == nil {
		return f, true
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		if s == "" || s == "-" {
			return 0, false
		}
		if v, err := strconv.ParseFloat(s, 64); err == nil {
			return v, true
		}
	}
	return 0, false
}

func (e *EastMoneyAdapter) GetQuote(ctx context.Context, market, symbol string) (*Quote, error) {
	if market != "cn" {
		return nil, ErrNotSupported // 骨架阶段东财仅打通 A 股
	}
	secid, ok := cnSecid(symbol)
	if !ok {
		return nil, ErrSymbolInvalid
	}

	url := fmt.Sprintf(
		"https://%d.push2.eastmoney.com/api/qt/stock/get?secid=%s&fields=f43,f44,f45,f46,f47,f48,f57,f58,f60,f86,f169,f170&invt=2&fltt=2",
		emNode(), secid,
	)
	body, status, err := doGet(ctx, url, map[string]string{"Referer": "https://quote.eastmoney.com/"})
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUpstream, err)
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("%w: http %d", ErrUpstream, status)
	}

	var parsed emQuoteResp
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("%w: 解析失败 %v", ErrUpstream, err)
	}
	if len(parsed.Data) == 0 {
		return nil, ErrNoData
	}

	d := parsed.Data
	price, ok := emNum(d["f43"])
	if !ok {
		return nil, ErrNoData // 现价拿不到视为无有效行情（停牌/非交易标的）
	}
	name := ""
	if v, ok := d["f58"]; ok {
		_ = json.Unmarshal(v, &name)
	}
	high, _ := emNum(d["f44"])
	low, _ := emNum(d["f45"])
	open, _ := emNum(d["f46"])
	vol, _ := emNum(d["f47"])
	amount, _ := emNum(d["f48"])
	prevClose, _ := emNum(d["f60"])
	changePct, _ := emNum(d["f170"])
	// f86 为行情时间戳（unix 秒）。types.go 契约要求数据时间随数据透传——
	// 休市时段取到的旧价若打上「现在」的时间戳，会被 AI 上下文当成实时价。
	dataTime := time.Now()
	if ts, ok := emNum(d["f86"]); ok && ts > 0 {
		dataTime = time.Unix(int64(ts), 0)
	}

	return &Quote{
		Symbol:    symbol,
		Market:    market,
		Name:      strings.TrimSpace(name),
		Price:     price,
		ChangePct: changePct,
		Open:      open,
		High:      high,
		Low:       low,
		PrevClose: prevClose,
		Volume:    int64(vol),
		Amount:    amount,
		Source:    e.Name(),
		DataTime:  dataTime,
	}, nil
}

// emKlineResp push2his 日线返回结构。
type emKlineResp struct {
	Data struct {
		Klines []string `json:"klines"`
	} `json:"data"`
}

func (e *EastMoneyAdapter) GetDailyBars(ctx context.Context, market, symbol string, limit int) ([]Bar, error) {
	if market != "cn" {
		return nil, ErrNotSupported
	}
	secid, ok := cnSecid(symbol)
	if !ok {
		return nil, ErrSymbolInvalid
	}
	if limit <= 0 {
		limit = 120
	}

	// klt=101 日线，fqt=1 前复权。
	url := fmt.Sprintf(
		"https://%d.push2his.eastmoney.com/api/qt/stock/kline/get?secid=%s&fields1=f1&fields2=f51,f52,f53,f54,f55,f56,f57&klt=101&fqt=1&end=20500101&lmt=%d",
		emNode(), secid, limit,
	)
	body, status, err := doGet(ctx, url, map[string]string{"Referer": "https://quote.eastmoney.com/"})
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUpstream, err)
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("%w: http %d", ErrUpstream, status)
	}

	var parsed emKlineResp
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("%w: 解析失败 %v", ErrUpstream, err)
	}

	bars := make([]Bar, 0, len(parsed.Data.Klines))
	for _, line := range parsed.Data.Klines {
		// 格式：date,open,close,high,low,volume,amount
		parts := strings.Split(line, ",")
		if len(parts) < 7 {
			continue
		}
		atof := func(s string) float64 { f, _ := strconv.ParseFloat(s, 64); return f }
		bars = append(bars, Bar{
			TradeDate: parts[0],
			Open:      atof(parts[1]),
			Close:     atof(parts[2]),
			High:      atof(parts[3]),
			Low:       atof(parts[4]),
			Volume:    int64(atof(parts[5])),
			Amount:    atof(parts[6]),
		})
	}
	if len(bars) == 0 {
		return nil, ErrNoData
	}
	return bars, nil
}

// GetSectorRanking 东财 clist 行业板块涨跌榜（best-effort：东财限流时常返回空，调用方降级处理）。
func (e *EastMoneyAdapter) GetSectorRanking(ctx context.Context, market string, limit int) ([]SectorRank, error) {
	if market != "cn" {
		return nil, ErrNotSupported
	}
	if limit <= 0 || limit > 100 {
		limit = 10
	}
	url := fmt.Sprintf(
		"https://%d.push2.eastmoney.com/api/qt/clist/get?pn=1&pz=%d&po=1&fid=f3&fltt=2&fs=m:90+t:2&fields=f12,f14,f3,f128",
		emNode(), limit,
	)
	body, status, err := doGet(ctx, url, map[string]string{"Referer": "https://quote.eastmoney.com/"})
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUpstream, err)
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("%w: http %d", ErrUpstream, status)
	}

	// diff 可能是数组或以 "0","1" 为键的对象，两种都兼容。
	var parsed struct {
		Data struct {
			Diff json.RawMessage `json:"diff"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("%w: 解析失败 %v", ErrUpstream, err)
	}

	type emSector struct {
		F12  string  `json:"f12"`
		F14  string  `json:"f14"`
		F3   float64 `json:"f3"`
		F128 string  `json:"f128"`
	}
	var items []emSector
	if len(parsed.Data.Diff) > 0 && parsed.Data.Diff[0] == '[' {
		_ = json.Unmarshal(parsed.Data.Diff, &items)
	} else if len(parsed.Data.Diff) > 0 {
		m := map[string]emSector{}
		if json.Unmarshal(parsed.Data.Diff, &m) == nil {
			keys := make([]int, 0, len(m))
			idx := map[int]string{}
			for k := range m {
				n, _ := strconv.Atoi(k)
				keys = append(keys, n)
				idx[n] = k
			}
			sort.Ints(keys)
			for _, n := range keys {
				items = append(items, m[idx[n]])
			}
		}
	}
	if len(items) == 0 {
		return nil, ErrNoData
	}

	out := make([]SectorRank, 0, len(items))
	for _, it := range items {
		out = append(out, SectorRank{
			Code: it.F12, Name: it.F14, ChangePct: it.F3, Leader: it.F128, Source: e.Name(),
		})
	}
	return out, nil
}

// GetBreadth 东财涨跌分布（getTopicZDFenBu）：一次调用返回全市场涨跌家数/涨跌停。
// data.fenbu 为 [{"桶":家数}] 数组，桶键：正数=上涨区间(11=涨停)、负数=下跌区间(-11=跌停)、0=平盘。
func (e *EastMoneyAdapter) GetBreadth(ctx context.Context, market string) (*Breadth, error) {
	if market != "cn" {
		return nil, ErrNotSupported
	}
	url := "https://push2ex.eastmoney.com/getTopicZDFenBu?ut=7eea3edcaed734bea9cbfc24409ed989&dpt=wz.ztzt"
	body, status, err := doGet(ctx, url, map[string]string{"Referer": "https://quote.eastmoney.com/"})
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUpstream, err)
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("%w: http %d", ErrUpstream, status)
	}
	b, err := parseBreadth(body)
	if err != nil {
		return nil, err
	}
	b.Source = e.Name()
	b.DataTime = time.Now()
	return b, nil
}

// parseBreadth 解析涨跌分布 JSON（抽出便于单测，防上游字段漂移）。
func parseBreadth(body []byte) (*Breadth, error) {
	var parsed struct {
		Data struct {
			QDate int64            `json:"qdate"`
			Fenbu []map[string]int `json:"fenbu"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("%w: 解析失败 %v", ErrUpstream, err)
	}
	if len(parsed.Data.Fenbu) == 0 {
		return nil, ErrNoData
	}
	b := &Breadth{}
	for _, bucket := range parsed.Data.Fenbu {
		for k, cnt := range bucket {
			n, err := strconv.Atoi(k)
			if err != nil {
				continue
			}
			switch {
			case n > 0:
				b.Advances += cnt
				if n == 11 { // 11 号桶为涨停
					b.LimitUp += cnt
				}
			case n < 0:
				b.Declines += cnt
				if n == -11 { // -11 号桶为跌停
					b.LimitDown += cnt
				}
			default:
				b.Unchanged += cnt
			}
		}
	}
	if b.Advances+b.Declines+b.Unchanged == 0 {
		return nil, ErrNoData
	}
	if parsed.Data.QDate > 0 {
		s := strconv.FormatInt(parsed.Data.QDate, 10)
		if len(s) == 8 {
			b.TradeDate = s[:4] + "-" + s[4:6] + "-" + s[6:]
		}
	}
	return b, nil
}

// GetMarketFundFlow 东财两市资金流（fflow/kline，沪+深合并）：取最新交易日主力/各单净流入。
// klines 每行：date,主力(f52),小单(f53),中单(f54),大单(f55),超大单(f56)，单位元。
func (e *EastMoneyAdapter) GetMarketFundFlow(ctx context.Context, market string) (*MarketFundFlow, error) {
	if market != "cn" {
		return nil, ErrNotSupported
	}
	url := "https://push2.eastmoney.com/api/qt/stock/fflow/kline/get?lmt=5&klt=101&secid=1.000001&secid2=0.399001" +
		"&fields1=f1,f2,f3,f7&fields2=f51,f52,f53,f54,f55,f56"
	body, status, err := doGet(ctx, url, map[string]string{"Referer": "https://quote.eastmoney.com/"})
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUpstream, err)
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("%w: http %d", ErrUpstream, status)
	}
	f, err := parseMarketFundFlow(body)
	if err != nil {
		return nil, err
	}
	f.Source = e.Name()
	f.DataTime = time.Now()
	return f, nil
}

// parseMarketFundFlow 解析资金流 JSON 的最新一行（抽出便于单测）。
func parseMarketFundFlow(body []byte) (*MarketFundFlow, error) {
	var parsed struct {
		Data struct {
			Klines []string `json:"klines"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("%w: 解析失败 %v", ErrUpstream, err)
	}
	if len(parsed.Data.Klines) == 0 {
		return nil, ErrNoData
	}
	line := parsed.Data.Klines[len(parsed.Data.Klines)-1] // 最新交易日
	parts := strings.Split(line, ",")
	if len(parts) < 6 {
		return nil, fmt.Errorf("%w: 资金流字段不全", ErrUpstream)
	}
	atof := func(s string) float64 { v, _ := strconv.ParseFloat(strings.TrimSpace(s), 64); return v }
	return &MarketFundFlow{
		TradeDate: parts[0],
		MainNet:   atof(parts[1]),
		SmallNet:  atof(parts[2]),
		MediumNet: atof(parts[3]),
		LargeNet:  atof(parts[4]),
		SuperNet:  atof(parts[5]),
	}, nil
}
