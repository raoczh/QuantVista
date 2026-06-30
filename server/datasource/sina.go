package datasource

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"golang.org/x/text/encoding/simplifiedchinese"
	"golang.org/x/text/transform"
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
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	req.Header.Set("User-Agent", browserUA)
	req.Header.Set("Referer", "https://finance.sina.com.cn") // 必须，否则被拒

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUpstream, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: http %d", ErrUpstream, resp.StatusCode)
	}

	// GBK -> UTF-8
	reader := transform.NewReader(io.LimitReader(resp.Body, 1<<20), simplifiedchinese.GBK.NewDecoder())
	raw, err := io.ReadAll(reader)
	if err != nil {
		return nil, fmt.Errorf("%w: 转码失败 %v", ErrUpstream, err)
	}

	// 形如：var hq_str_sh600000="浦发银行,9.98,9.95,10.04,10.10,9.90,...";
	text := string(raw)
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

// GetDailyBars 新浪日线接口格式杂乱，骨架阶段不实现，由 manager 回退到东财。
func (s *SinaAdapter) GetDailyBars(ctx context.Context, market, symbol string, limit int) ([]Bar, error) {
	return nil, ErrNotSupported
}
