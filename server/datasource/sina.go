package datasource

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"golang.org/x/text/encoding/simplifiedchinese"
)

// SinaAdapter 新浪财经 hq.sinajs.cn，作为东财的备份/交叉校验源。
// 注意：必须带 Referer，否则被拒；返回 GBK 文本需转码。
type SinaAdapter struct{}

func NewSinaAdapter() *SinaAdapter { return &SinaAdapter{} }

func (s *SinaAdapter) Name() string { return "sina" }

func (s *SinaAdapter) GetQuote(ctx context.Context, market, symbol string) (*Quote, error) {
	if market != "cn" {
		return nil, ErrNotSupported
	}
	code, ok := sinaCNSymbol(symbol)
	if !ok {
		return nil, ErrSymbolInvalid
	}

	url := "https://hq.sinajs.cn/list=" + code
	// 新浪必须带 Referer，否则被拒；返回 GBK 文本。
	raw, status, err := doGet(ctx, url, map[string]string{"Referer": "https://finance.sina.com.cn"})
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUpstream, err)
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("%w: http %d", ErrUpstream, status)
	}

	// GBK -> UTF-8
	decoded, err := simplifiedchinese.GBK.NewDecoder().Bytes(raw)
	if err != nil {
		return nil, fmt.Errorf("%w: 转码失败 %v", ErrUpstream, err)
	}

	// 形如：var hq_str_sh600000="浦发银行,9.98,9.95,10.04,10.10,9.90,...";
	text := string(decoded)
	idx := strings.Index(text, "\"")
	if idx < 0 {
		return nil, ErrNoData
	}
	end := strings.LastIndex(text, "\"")
	if end <= idx {
		return nil, ErrNoData
	}
	fields := strings.Split(text[idx+1:end], ",")
	if len(fields) < 32 {
		return nil, ErrNoData // 停牌或代码无效时字段不全
	}

	atof := func(i int) float64 {
		if i >= len(fields) {
			return 0
		}
		f, _ := strconv.ParseFloat(strings.TrimSpace(fields[i]), 64)
		return f
	}

	price := atof(3)
	if price == 0 {
		return nil, ErrNoData
	}
	prevClose := atof(2)
	changePct := 0.0
	if prevClose != 0 {
		changePct = (price - prevClose) / prevClose * 100
	}

	// 字段 30/31 为日期/时间（北京时间）。
	dataTime := time.Now()
	if t, err := time.ParseInLocation("2006-01-02 15:04:05", fields[30]+" "+fields[31], time.Local); err == nil {
		dataTime = t
	}

	return &Quote{
		Symbol:    symbol,
		Market:    market,
		Name:      strings.TrimSpace(fields[0]),
		Price:     price,
		ChangePct: changePct,
		Open:      atof(1),
		High:      atof(4),
		Low:       atof(5),
		PrevClose: prevClose,
		Volume:    int64(atof(8) / 100), // 新浪返回股，统一为手（与东财/腾讯口径一致）
		Amount:    atof(9),
		Source:    s.Name(),
		DataTime:  dataTime,
	}, nil
}

// GetIndices 用新浪批量接口一次拉取主要指数（市场首页指数概览）。
func (s *SinaAdapter) GetIndices(ctx context.Context, market string) ([]Index, error) {
	if market != "cn" {
		return nil, ErrNotSupported
	}
	codes := make([]string, 0, len(CNIndices))
	for _, ix := range CNIndices {
		codes = append(codes, ix.Sina)
	}
	url := "https://hq.sinajs.cn/list=" + strings.Join(codes, ",")
	raw, status, err := doGet(ctx, url, map[string]string{"Referer": "https://finance.sina.com.cn"})
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUpstream, err)
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("%w: http %d", ErrUpstream, status)
	}
	decoded, err := simplifiedchinese.GBK.NewDecoder().Bytes(raw)
	if err != nil {
		return nil, fmt.Errorf("%w: 转码失败 %v", ErrUpstream, err)
	}

	// 每行：var hq_str_sh000001="上证指数,open,prevclose,price,high,low,...,date,time";
	// 按行内 hq_str_<code> 与 CNIndices 匹配——不能按行号对位，响应漏行/插行时会
	// 把深成指的数值标成上证指数。
	byCode := map[string][]string{}
	for _, l := range strings.Split(string(decoded), "\n") {
		m := strings.Index(l, "hq_str_")
		a := strings.Index(l, "\"")
		b := strings.LastIndex(l, "\"")
		if m < 0 || a < 0 || b <= a {
			continue
		}
		code := strings.TrimSpace(strings.TrimSuffix(l[m+len("hq_str_"):a], "="))
		byCode[code] = strings.Split(l[a+1:b], ",")
	}
	out := make([]Index, 0, len(CNIndices))
	for _, ix := range CNIndices {
		f, ok := byCode[ix.Sina]
		if !ok || len(f) < 6 {
			continue
		}
		atof := func(idx int) float64 { v, _ := strconv.ParseFloat(strings.TrimSpace(f[idx]), 64); return v }
		price := atof(3)
		prevClose := atof(2)
		if price == 0 {
			continue
		}
		pct := 0.0
		if prevClose != 0 {
			pct = (price - prevClose) / prevClose * 100
		}
		out = append(out, Index{
			Code: ix.Code, Name: ix.Name,
			Price: price, ChangePct: pct,
			Open: atof(1), High: atof(4), Low: atof(5), PrevClose: prevClose,
			Source: s.Name(), DataTime: time.Now(),
		})
	}
	if len(out) == 0 {
		return nil, ErrNoData
	}
	return out, nil
}

// GetStockRanking 用新浪 Market_Center 榜单接口。
// sort: changepercent / amount / turnoverratio / pb；asc=true 升序（跌幅榜、低PB榜）。
// 实测注意：per 升序时负 PE（亏损股）整段排在最前无法翻越，故不放行 per；
// pb 升序前排仅个别退市股为负值，由调用方本地过滤 pb>0。
func (s *SinaAdapter) GetStockRanking(ctx context.Context, market, sort string, asc bool, limit int) ([]StockRank, error) {
	if market != "cn" {
		return nil, ErrNotSupported
	}
	if limit <= 0 || limit > 100 {
		limit = 10
	}
	// 白名单钳制：上游同一接口还支持更多排序字段，按需扩枚举即可（非新数据源）。
	switch sort {
	case "changepercent", "amount", "turnoverratio", "pb":
	default:
		sort = "changepercent"
	}
	ascFlag := 0
	if asc {
		ascFlag = 1
	}
	url := fmt.Sprintf(
		"https://vip.stock.finance.sina.com.cn/quotes_service/api/json_v2.php/Market_Center.getHQNodeData?node=hs_a&sort=%s&asc=%d&num=%d&page=1",
		sort, ascFlag, limit,
	)
	body, status, err := doGet(ctx, url, map[string]string{"Referer": "https://finance.sina.com.cn"})
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUpstream, err)
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("%w: http %d", ErrUpstream, status)
	}

	var rows []struct {
		Code          string  `json:"code"`
		Name          string  `json:"name"`
		Trade         string  `json:"trade"`
		Changepercent float64 `json:"changepercent"`
		Amount        float64 `json:"amount"`
		Turnoverratio float64 `json:"turnoverratio"`
		PER           float64 `json:"per"`
		PB            float64 `json:"pb"`
		NMC           float64 `json:"nmc"` // 流通市值（万元）
	}
	if err := json.Unmarshal(body, &rows); err != nil {
		return nil, fmt.Errorf("%w: 解析失败 %v", ErrUpstream, err)
	}
	if len(rows) == 0 {
		return nil, ErrNoData
	}
	out := make([]StockRank, 0, len(rows))
	for _, r := range rows {
		price, _ := strconv.ParseFloat(r.Trade, 64)
		out = append(out, StockRank{
			Symbol: r.Code, Name: r.Name, Price: price,
			ChangePct: r.Changepercent, Amount: r.Amount, TurnoverRate: r.Turnoverratio,
			PE: r.PER, PB: r.PB, FloatCap: r.NMC * 1e4, // 万元 → 元
			Source: s.Name(),
		})
	}
	return out, nil
}

// 返回按日期升序的日线。注意：该接口无复权参数（口径与东财 fqt=1 前复权不一定
// 一致），仅作东财日线失败时的兜底；不提供成交额，Amount 置 0（落库时不覆盖已有值）。
func (s *SinaAdapter) GetDailyBars(ctx context.Context, market, symbol string, limit int) ([]Bar, error) {
	if market != "cn" {
		return nil, ErrNotSupported
	}
	code, ok := sinaCNSymbol(symbol)
	if !ok {
		return nil, ErrSymbolInvalid
	}
	if limit <= 0 {
		limit = 120
	}

	// scale=240 表示日线（分钟数），ma=no 不返回均线。
	url := fmt.Sprintf(
		"https://money.finance.sina.com.cn/quotes_service/api/json_v2.php/CN_MarketData.getKLineData?symbol=%s&scale=240&ma=no&datalen=%d",
		code, limit,
	)
	body, status, err := doGet(ctx, url, map[string]string{"Referer": "https://finance.sina.com.cn"})
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUpstream, err)
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("%w: http %d", ErrUpstream, status)
	}

	var rows []struct {
		Day    string `json:"day"`
		Open   string `json:"open"`
		High   string `json:"high"`
		Low    string `json:"low"`
		Close  string `json:"close"`
		Volume string `json:"volume"`
	}
	if err := json.Unmarshal(body, &rows); err != nil {
		return nil, fmt.Errorf("%w: 解析失败 %v", ErrUpstream, err)
	}
	if len(rows) == 0 {
		return nil, ErrNoData
	}

	atof := func(s string) float64 { f, _ := strconv.ParseFloat(s, 64); return f }
	bars := make([]Bar, 0, len(rows))
	for _, r := range rows {
		bars = append(bars, Bar{
			TradeDate: r.Day,
			Open:      atof(r.Open),
			High:      atof(r.High),
			Low:       atof(r.Low),
			Close:     atof(r.Close),
			Volume:    int64(atof(r.Volume) / 100), // 新浪返回股，统一为手（与东财口径一致）
			Amount:    0, // 该接口不提供成交额
		})
	}
	return bars, nil
}

// GetBenchmarkBars 取基准指数日线（cn=上证指数 sh000001），供推荐追踪计算超额收益/alpha。
// 复用与 GetTradingDays 相同的上证指数 KLine 接口，但保留完整 OHLC；返回按日期升序。
func (s *SinaAdapter) GetBenchmarkBars(ctx context.Context, market string, limit int) (string, []Bar, error) {
	if market != "cn" {
		return "", nil, ErrNotSupported
	}
	if limit <= 0 || limit > 1023 {
		limit = 250
	}
	url := fmt.Sprintf(
		"https://money.finance.sina.com.cn/quotes_service/api/json_v2.php/CN_MarketData.getKLineData?symbol=sh000001&scale=240&ma=no&datalen=%d",
		limit,
	)
	body, status, err := doGet(ctx, url, map[string]string{"Referer": "https://finance.sina.com.cn"})
	if err != nil {
		return "", nil, fmt.Errorf("%w: %v", ErrUpstream, err)
	}
	if status != http.StatusOK {
		return "", nil, fmt.Errorf("%w: http %d", ErrUpstream, status)
	}
	var rows []struct {
		Day   string `json:"day"`
		Open  string `json:"open"`
		High  string `json:"high"`
		Low   string `json:"low"`
		Close string `json:"close"`
	}
	if err := json.Unmarshal(body, &rows); err != nil {
		return "", nil, fmt.Errorf("%w: 解析失败 %v", ErrUpstream, err)
	}
	if len(rows) == 0 {
		return "", nil, ErrNoData
	}
	atof := func(s string) float64 { f, _ := strconv.ParseFloat(s, 64); return f }
	bars := make([]Bar, 0, len(rows))
	for _, r := range rows {
		bars = append(bars, Bar{
			TradeDate: r.Day,
			Open:      atof(r.Open),
			High:      atof(r.High),
			Low:       atof(r.Low),
			Close:     atof(r.Close),
			Source:    s.Name(),
		})
	}
	return "上证指数", bars, nil
}
// 返回按日期升序的 YYYY-MM-DD 列表。
func (s *SinaAdapter) GetTradingDays(ctx context.Context, market string, limit int) ([]string, error) {
	if market != "cn" {
		return nil, ErrNotSupported
	}
	if limit <= 0 || limit > 1023 {
		limit = 1000
	}
	url := fmt.Sprintf(
		"https://money.finance.sina.com.cn/quotes_service/api/json_v2.php/CN_MarketData.getKLineData?symbol=sh000001&scale=240&ma=no&datalen=%d",
		limit,
	)
	body, status, err := doGet(ctx, url, map[string]string{"Referer": "https://finance.sina.com.cn"})
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUpstream, err)
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("%w: http %d", ErrUpstream, status)
	}
	var rows []struct {
		Day string `json:"day"`
	}
	if err := json.Unmarshal(body, &rows); err != nil {
		return nil, fmt.Errorf("%w: 解析失败 %v", ErrUpstream, err)
	}
	if len(rows) == 0 {
		return nil, ErrNoData
	}
	days := make([]string, 0, len(rows))
	for _, r := range rows {
		if r.Day != "" {
			days = append(days, r.Day)
		}
	}
	return days, nil
}
