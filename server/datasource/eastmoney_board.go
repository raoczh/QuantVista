package datasource

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// 东财板块（clist）——M3c 行业热力图 + 板块详情页数据源。
// 板块热度：fs=m:90+t:2（行业）/ m:90+t:3（概念），各约 496 个、两类互不重叠；
// fid=f6 按成交额降序取 Top100 单页（pz 上游硬钳 100，热力图可读性足够）。
// 成分股：fs=b:BK1036，fid=f6 降序单页。板块指数日线：secid=90.BK1036，与个股 kline 同口径。
const (
	boardHeatFields   = "f12,f14,f3,f6,f104,f105,f128,f140"
	boardHeatPageSize = 100
	boardStockFields  = "f12,f14,f2,f3,f6,f8,f20,f21"
	boardListMaxPages = 10 // 行业/概念各约 496 个 ≈ 5 页，留余量防上游 total 异常翻页失控
	boardListPageGap  = 200 * time.Millisecond
)

// boardFS 板块种类白名单 → clist fs 过滤串。
var boardFS = map[string]string{
	"industry": "m:90+t:2",
	"concept":  "m:90+t:3",
}

// bkCodeRe 板块代码校验（BK 后 4 位数字），进 URL 前必须通过以防注入。
var bkCodeRe = regexp.MustCompile(`^BK\d{4}$`)

// GetBoardHeat 拉取行业/概念板块热度榜（Top100，按成交额降序）。走 e.get（push2 断路器）。
func (e *EastMoneyAdapter) GetBoardHeat(ctx context.Context, kind string) ([]BoardHeat, error) {
	fs, ok := boardFS[kind]
	if !ok {
		return nil, ErrSymbolInvalid
	}
	url := fmt.Sprintf(
		"https://%d.push2.eastmoney.com/api/qt/clist/get?pn=1&pz=%d&po=1&np=1&fid=f6&fltt=2&invt=2&ut=%s&fs=%s&fields=%s",
		emNode(), boardHeatPageSize, clistUT, fs, boardHeatFields,
	)
	body, status, err := e.get(ctx, url, map[string]string{"Referer": "https://quote.eastmoney.com/"})
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUpstream, err)
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("%w: http %d", ErrUpstream, status)
	}
	rows, err := parseBoardHeat(body)
	if err != nil {
		return nil, err
	}
	for i := range rows {
		rows[i].Source = e.Name()
	}
	return rows, nil
}

// GetBoardConstituents 拉取板块成分股（按成交额降序，limit 上限由上游 pz 硬钳 100）。
func (e *EastMoneyAdapter) GetBoardConstituents(ctx context.Context, code string, limit int) ([]BoardStock, error) {
	code = strings.ToUpper(strings.TrimSpace(code))
	if !bkCodeRe.MatchString(code) {
		return nil, ErrSymbolInvalid
	}
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	url := fmt.Sprintf(
		"https://%d.push2.eastmoney.com/api/qt/clist/get?pn=1&pz=%d&po=1&np=1&fid=f6&fltt=2&invt=2&ut=%s&fs=b:%s&fields=%s",
		emNode(), limit, clistUT, code, boardStockFields,
	)
	body, status, err := e.get(ctx, url, map[string]string{"Referer": "https://quote.eastmoney.com/"})
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUpstream, err)
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("%w: http %d", ErrUpstream, status)
	}
	rows, err := parseBoardStocks(body)
	if err != nil {
		return nil, err
	}
	for i := range rows {
		rows[i].Source = e.Name()
	}
	return rows, nil
}

// GetBoardKline 拉取板块指数日线（前复权），复用个股 kline 解析逻辑。
// cnSecid 不认 BK 码，这里自行拼 secid=90.<code>（板块指数固定在 90 市场）。
func (e *EastMoneyAdapter) GetBoardKline(ctx context.Context, code string, limit int) ([]Bar, error) {
	code = strings.ToUpper(strings.TrimSpace(code))
	if !bkCodeRe.MatchString(code) {
		return nil, ErrSymbolInvalid
	}
	if limit <= 0 {
		limit = 120
	}
	url := fmt.Sprintf(
		"https://%d.push2his.eastmoney.com/api/qt/stock/kline/get?secid=90.%s&fields1=f1&fields2=f51,f52,f53,f54,f55,f56,f57,f58,f59,f60,f61&klt=101&fqt=1&end=20500101&lmt=%d",
		emNode(), code, limit,
	)
	body, status, err := e.get(ctx, url, map[string]string{"Referer": "https://quote.eastmoney.com/"})
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
	bars := parseEMKlines(parsed.Data.Klines)
	if len(bars) == 0 {
		return nil, ErrNoData
	}
	for i := range bars {
		bars[i].Source = e.Name()
	}
	return bars, nil
}

// GetBoardFundFlow 板块资金流逐日历史（升序，P3b）。与个股 fflow/daykline 同一接口、
// 同 15 列结构（2026-07-10 实测 secid=90.BK1036：date,主力,小,中,大,超大,5×占比%,
// Close=板块指数点位,涨跌幅,0,0），直接复用 parseStockFundFlow（同包，勿复制粘贴）。
// 走 push2his（无备用域，断路器熔断纪律同个股；本机限流属常态，LIVE 冒烟留部署环境）。
func (e *EastMoneyAdapter) GetBoardFundFlow(ctx context.Context, code string, limit int) ([]StockFundFlowBar, error) {
	code = strings.ToUpper(strings.TrimSpace(code))
	if !bkCodeRe.MatchString(code) {
		return nil, ErrSymbolInvalid
	}
	if limit <= 0 || limit > 1000 {
		limit = 250
	}
	url := fmt.Sprintf(
		"https://%d.push2his.eastmoney.com/api/qt/stock/fflow/daykline/get?secid=90.%s&lmt=%d&klt=101&fields1=f1,f2,f3,f7&fields2=f51,f52,f53,f54,f55,f56,f57,f58,f59,f60,f61,f62,f63,f64,f65&ut=%s",
		emNode(), code, limit, fflowUT,
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

// BoardListItem 板块清单轻行（P3b 估值聚合的行业名→BK 码映射用）。
type BoardListItem struct {
	Code string `json:"code"`
	Name string `json:"name"`
}

// GetBoardList 拉全某类板块的代码与名称（f12,f14 轻字段；fid=f12 升序稳定翻页，
// 行业约 496 个 ≈ 5 页）。翻页纪律同 GetCNSpotSnapshot：半截清单拒收（估值聚合
// 用它建行业名→BK 码映射，缺一半会让一半行业静默跳过）。
func (e *EastMoneyAdapter) GetBoardList(ctx context.Context, kind string) ([]BoardListItem, error) {
	fs, ok := boardFS[kind]
	if !ok {
		return nil, ErrSymbolInvalid
	}
	all := make([]BoardListItem, 0, 512)
	total := 0
	for pn := 1; pn <= boardListMaxPages; pn++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		url := fmt.Sprintf(
			"https://%d.push2.eastmoney.com/api/qt/clist/get?pn=%d&pz=%d&po=0&np=1&fid=f12&fltt=2&invt=2&ut=%s&fs=%s&fields=f12,f14",
			emNode(), pn, boardHeatPageSize, clistUT, fs,
		)
		body, status, err := e.get(ctx, url, map[string]string{"Referer": "https://quote.eastmoney.com/"})
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrUpstream, err)
		}
		if status != http.StatusOK {
			return nil, fmt.Errorf("%w: http %d", ErrUpstream, status)
		}
		rows, pageTotal, err := parseBoardList(body)
		if err != nil {
			return nil, err
		}
		if pageTotal > 0 {
			total = pageTotal
		}
		if len(rows) == 0 {
			break // 空页=翻完
		}
		all = append(all, rows...)
		if total > 0 && len(all) >= total {
			break
		}
		time.Sleep(boardListPageGap)
	}
	if len(all) == 0 {
		return nil, ErrNoData
	}
	if total > 0 && len(all) < total*9/10 {
		return nil, fmt.Errorf("%w: 板块清单不完整 %d/%d", ErrUpstream, len(all), total)
	}
	return all, nil
}

// parseBoardList 解析板块清单一页（抽出便于单测）。返回 (rows, total, err)。
func parseBoardList(body []byte) ([]BoardListItem, int, error) {
	var parsed struct {
		Data struct {
			Total int `json:"total"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, 0, fmt.Errorf("%w: 解析失败 %v", ErrUpstream, err)
	}
	items, err := clistDiffItems(body)
	if err != nil {
		return nil, 0, err
	}
	rows := make([]BoardListItem, 0, len(items))
	for _, it := range items {
		var b struct {
			F12 string `json:"f12"`
			F14 string `json:"f14"`
		}
		if json.Unmarshal(it, &b) != nil {
			continue
		}
		code := strings.TrimSpace(b.F12)
		if code == "" {
			continue
		}
		rows = append(rows, BoardListItem{Code: code, Name: strings.TrimSpace(b.F14)})
	}
	return rows, parsed.Data.Total, nil
}

// clistDiffItems 解析 clist 的 data.diff 为原始 map 序列（兼容数组与 {"0":{...}} 对象两形态，
// 全项目通用坑——同 parseClistSpot/GetSectorRanking）。对象态按数字键还原顺序。
func clistDiffItems(body []byte) ([]json.RawMessage, error) {
	var parsed struct {
		Data struct {
			Diff json.RawMessage `json:"diff"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("%w: 解析失败 %v", ErrUpstream, err)
	}
	raw := parsed.Data.Diff
	if len(raw) == 0 {
		return nil, nil
	}
	if raw[0] == '[' {
		var items []json.RawMessage
		if err := json.Unmarshal(raw, &items); err != nil {
			return nil, fmt.Errorf("%w: 解析失败 %v", ErrUpstream, err)
		}
		return items, nil
	}
	if raw[0] == '{' {
		m := map[string]json.RawMessage{}
		if err := json.Unmarshal(raw, &m); err != nil {
			return nil, fmt.Errorf("%w: 解析失败 %v", ErrUpstream, err)
		}
		keys := make([]int, 0, len(m))
		idx := map[int]string{}
		for k := range m {
			n, err := strconv.Atoi(k)
			if err != nil {
				continue
			}
			keys = append(keys, n)
			idx[n] = k
		}
		sort.Ints(keys)
		items := make([]json.RawMessage, 0, len(keys))
		for _, n := range keys {
			items = append(items, m[idx[n]])
		}
		return items, nil
	}
	return nil, nil
}

// parseBoardHeat 解析板块热度榜一页（抽出便于单测，字段缺失/"-" 容错为 0）。
func parseBoardHeat(body []byte) ([]BoardHeat, error) {
	items, err := clistDiffItems(body)
	if err != nil {
		return nil, err
	}
	type emBoard struct {
		F12  string          `json:"f12"`
		F14  string          `json:"f14"`
		F3   json.RawMessage `json:"f3"`
		F6   json.RawMessage `json:"f6"`
		F104 json.RawMessage `json:"f104"`
		F105 json.RawMessage `json:"f105"`
		F128 string          `json:"f128"`
		F140 string          `json:"f140"`
	}
	rows := make([]BoardHeat, 0, len(items))
	for _, it := range items {
		var b emBoard
		if json.Unmarshal(it, &b) != nil {
			continue
		}
		code := strings.TrimSpace(b.F12)
		if code == "" {
			continue
		}
		pct, _ := emNum(b.F3)
		amount, _ := emNum(b.F6)
		adv, _ := emNum(b.F104)
		dec, _ := emNum(b.F105)
		rows = append(rows, BoardHeat{
			Code:       code,
			Name:       strings.TrimSpace(b.F14),
			ChangePct:  pct,
			Amount:     amount,
			Advances:   int(adv),
			Declines:   int(dec),
			Leader:     strings.TrimSpace(b.F128),
			LeaderCode: strings.TrimSpace(b.F140),
		})
	}
	if len(rows) == 0 {
		return nil, ErrNoData
	}
	return rows, nil
}

// parseBoardStocks 解析板块成分股一页（抽出便于单测，价格字段 "-" 容错为 0）。
func parseBoardStocks(body []byte) ([]BoardStock, error) {
	items, err := clistDiffItems(body)
	if err != nil {
		return nil, err
	}
	type emStock struct {
		F12 string          `json:"f12"`
		F14 string          `json:"f14"`
		F2  json.RawMessage `json:"f2"`
		F3  json.RawMessage `json:"f3"`
		F6  json.RawMessage `json:"f6"`
		F8  json.RawMessage `json:"f8"`
		F20 json.RawMessage `json:"f20"`
		F21 json.RawMessage `json:"f21"`
	}
	rows := make([]BoardStock, 0, len(items))
	for _, it := range items {
		var s emStock
		if json.Unmarshal(it, &s) != nil {
			continue
		}
		sym := strings.TrimSpace(s.F12)
		if sym == "" {
			continue
		}
		price, _ := emNum(s.F2)
		pct, _ := emNum(s.F3)
		amount, _ := emNum(s.F6)
		turnover, _ := emNum(s.F8)
		totalCap, _ := emNum(s.F20)
		floatCap, _ := emNum(s.F21)
		rows = append(rows, BoardStock{
			Symbol:       sym,
			Name:         strings.TrimSpace(s.F14),
			Price:        price,
			ChangePct:    pct,
			Amount:       amount,
			TurnoverRate: turnover,
			TotalCap:     totalCap,
			FloatCap:     floatCap,
		})
	}
	if len(rows) == 0 {
		return nil, ErrNoData
	}
	return rows, nil
}
