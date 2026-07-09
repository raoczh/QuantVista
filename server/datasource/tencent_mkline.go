package datasource

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
)

// 腾讯 5 分钟线（M3b）：ifzq.gtimg.cn/appstock/app/kline/mkline，免鉴权。
// 上游实测锚点（2026-07-09）：
//   - m5 行 8 列 [时间YYYYMMDDHHmm, 开, 收, 高, 低, 量(手), {}, 分钟换手]——
//     列序是「开、收、高、低」（腾讯 kline 族惯例），第 7 列为空对象占位；
//   - 无成交额列，消费方以 量×典型价 估算（service/intraday.go 的 VWAP 口径）；
//   - 时间戳为 bar 结束时刻：一天 48 根，0935 首根（含集合竞价）~1500 末根（含收盘竞价）；
//   - count=800 实测可回溯约 18 个交易日；盘中请求末根是进行中的半截 bar；
//   - 非法代码返回 data.{code}.qt 空数组且无 m5 键（据此判 ErrNoData）；
//   - 停牌日直接缺该日的根（不会出现零价行）。

// min5MaxCount 单次请求根数上限（上游实测 800 可用，再大未验证）。
const min5MaxCount = 800

// GetMin5Bars 拉取 5 分钟线，按时间升序返回。count<=0 默认 60（覆盖当日 48 根+上日尾部）。
func (t *TencentAdapter) GetMin5Bars(ctx context.Context, market, symbol string, count int) ([]Min5Bar, error) {
	if market != "cn" {
		return nil, ErrNotSupported
	}
	code, ok := sinaCNSymbol(symbol) // 腾讯与新浪同为 sh/sz 前缀
	if !ok {
		return nil, ErrSymbolInvalid
	}
	if count <= 0 {
		count = 60
	}
	if count > min5MaxCount {
		count = min5MaxCount
	}
	url := fmt.Sprintf("https://ifzq.gtimg.cn/appstock/app/kline/mkline?param=%s,m5,,%d", code, count)
	raw, status, err := doGet(ctx, url, map[string]string{"Referer": "https://gu.qq.com/"})
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUpstream, err)
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("%w: http %d", ErrUpstream, status)
	}
	return parseMin5Response(raw, code)
}

// parseMin5Response 解析 mkline 响应（独立函数便于 fixture 单测）。
func parseMin5Response(raw []byte, code string) ([]Min5Bar, error) {
	var resp struct {
		Code int                        `json:"code"`
		Msg  string                     `json:"msg"`
		Data map[string]json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("%w: mkline 响应解析失败: %v", ErrUpstream, err)
	}
	if resp.Code != 0 {
		return nil, fmt.Errorf("%w: mkline code=%d msg=%s", ErrUpstream, resp.Code, resp.Msg)
	}
	entry, ok := resp.Data[code]
	if !ok {
		return nil, ErrNoData
	}
	var body struct {
		M5 [][]any `json:"m5"`
	}
	if err := json.Unmarshal(entry, &body); err != nil {
		return nil, fmt.Errorf("%w: mkline m5 解析失败: %v", ErrUpstream, err)
	}
	if len(body.M5) == 0 {
		return nil, ErrNoData // 非法代码/无数据：qt 空数组且无 m5 键
	}
	out := make([]Min5Bar, 0, len(body.M5))
	for _, row := range body.M5 {
		if len(row) < 6 {
			continue // 坏行跳过
		}
		tstr, _ := row[0].(string)
		if len(tstr) != 12 {
			continue
		}
		o, c := min5Atof(row[1]), min5Atof(row[2])
		h, l := min5Atof(row[3]), min5Atof(row[4])
		if o <= 0 || c <= 0 || h <= 0 || l <= 0 {
			continue // 价格缺失的脏行（停牌日上游直接缺根，正常数据不会出现）
		}
		out = append(out, Min5Bar{
			Time: tstr, Open: o, High: h, Low: l, Close: c,
			Volume: int64(min5Atof(row[5])),
		})
	}
	if len(out) == 0 {
		return nil, ErrNoData
	}
	return out, nil
}

// min5Atof m5 行元素转 float（上游数值以字符串下发，{} 占位列断言失败返回 0）。
func min5Atof(v any) float64 {
	s, ok := v.(string)
	if !ok {
		return 0
	}
	f, _ := strconv.ParseFloat(s, 64)
	return f
}
