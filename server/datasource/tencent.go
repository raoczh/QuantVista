package datasource

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"golang.org/x/text/encoding/simplifiedchinese"
)

// TencentAdapter 腾讯财经 qt.gtimg.cn 实时行情。
// 独立于东财/新浪的第三源，稳定性好；仅实现实时快照，日线回退到新浪。
type TencentAdapter struct{}

func NewTencentAdapter() *TencentAdapter { return &TencentAdapter{} }

func (t *TencentAdapter) Name() string { return "tencent" }

func (t *TencentAdapter) GetQuote(ctx context.Context, market, symbol string) (*Quote, error) {
	if market != "cn" {
		return nil, ErrNotSupported
	}
	code, ok := sinaCNSymbol(symbol) // 腾讯与新浪同为 sh/sz 前缀
	if !ok {
		return nil, ErrSymbolInvalid
	}

	url := "https://qt.gtimg.cn/q=" + code
	raw, status, err := doGet(ctx, url, map[string]string{"Referer": "https://gu.qq.com/"})
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

	// 形如：v_sh600000="1~浦发银行~600000~8.61~8.73~8.69~684341~...~涨跌幅~最高~最低~价/量/额~..."
	text := string(decoded)
	a := strings.Index(text, "\"")
	b := strings.LastIndex(text, "\"")
	if a < 0 || b <= a {
		return nil, ErrNoData
	}
	f := strings.Split(text[a+1:b], "~")
	if len(f) < 38 {
		return nil, ErrNoData // 停牌/非法代码字段不全
	}
	atof := func(i int) float64 {
		if i >= len(f) {
			return 0
		}
		v, _ := strconv.ParseFloat(strings.TrimSpace(f[i]), 64)
		return v
	}

	price := atof(3)
	if price == 0 {
		return nil, ErrNoData
	}
	// 成交额优先取 "价/量/额"（字段 35）的第三段（单位元），更精确。
	amount := 0.0
	if seg := strings.Split(f[35], "/"); len(seg) >= 3 {
		amount, _ = strconv.ParseFloat(seg[2], 64)
	}

	dataTime := time.Now()
	if tm, err := time.ParseInLocation("20060102150405", f[30], time.Local); err == nil {
		dataTime = tm
	}

	return &Quote{
		Symbol:    symbol,
		Market:    market,
		Name:      strings.TrimSpace(f[1]),
		Price:     price,
		ChangePct: atof(32),
		Open:      atof(5),
		High:      atof(33),
		Low:       atof(34),
		PrevClose: atof(4),
		Volume:    int64(atof(6)), // 手
		Amount:    amount,
		Source:    t.Name(),
		DataTime:  dataTime,
	}, nil
}

// GetDailyBars 腾讯日线接口字段不稳，骨架不实现，由 manager 回退到新浪日线。
func (t *TencentAdapter) GetDailyBars(ctx context.Context, market, symbol string, limit int) ([]Bar, error) {
	return nil, ErrNotSupported
}
