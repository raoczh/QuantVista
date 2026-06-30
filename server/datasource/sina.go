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
		Volume:    int64(atof(8)),
		Amount:    atof(9),
		Source:    s.Name(),
		DataTime:  dataTime,
	}, nil
}

// GetDailyBars 用新浪 money.finance 的 JSON 日线接口（东财 EOF 时的兜底日线源）。
// 返回按日期升序的前复权日线；该接口不提供成交额，Amount 置 0。
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
			Volume:    int64(atof(r.Volume)),
			Amount:    0, // 该接口不提供成交额
		})
	}
	return bars, nil
}
