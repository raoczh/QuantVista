package datasource

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
)

// 腾讯分钟线（M3b 盘中因子 m5 / A1 分时图 m1）：ifzq.gtimg.cn/appstock/app/kline/mkline，免鉴权。
// 上游实测锚点（m5 于 2026-07-09、m1 于 2026-07-27 实测）：
//   - 行 8 列 [时间YYYYMMDDHHmm, 开, 收, 高, 低, 量(手), {}, 分钟换手]——
//     列序是「开、收、高、低」（腾讯 kline 族惯例），第 7 列为空对象占位；
//     **m1 与 m5 逐列同构**，故两周期共用同一解析器（period 只决定取哪个键）；
//   - 无成交额列：m5 因子以 量×典型价 估算，m1 分时均价以 量×收盘价估算；
//   - 时间戳为 bar 结束时刻：m5 一天 48 根（0935 首根含集合竞价~1500 末根含收盘竞价）、
//     m1 一天 240 根（0931~1500）；
//   - m5 count=800 实测可回溯约 18 个交易日；盘中请求末根是进行中的半截 bar；
//   - 非法代码返回 data.{code}.qt 空数组且无 m1/m5 键（据此判 ErrNoData）；
//   - 停牌日直接缺该日的根（不会出现零价行）；
//   - `prec` 键为前收盘（字符串）——分时图基准线用，仅 minuteQuote 路径消费。
//
// **非交易时段返回的是最近交易日的根**（实测 2026-07-27 09:20 请求 m1 回 07-24 尾部），
// 消费方据末根日期自行判定归属交易日，不得假设恒为「今天」。

// min5MaxCount 单次请求根数上限（上游实测 800 可用，再大未验证）。
const min5MaxCount = 800

// minutePeriod 支持的分钟周期（上游 param 段取值）。
const (
	minutePeriodM1 = "m1"
	minutePeriodM5 = "m5"
)

// GetMin5Bars 拉取 5 分钟线，按时间升序返回。count<=0 默认 60（覆盖当日 48 根+上日尾部）。
func (t *TencentAdapter) GetMin5Bars(ctx context.Context, market, symbol string, count int) ([]Min5Bar, error) {
	if count <= 0 {
		count = 60
	}
	bars, _, err := t.getMinuteBars(ctx, market, symbol, minutePeriodM5, count)
	return bars, err
}

// GetMin1Bars 拉取 1 分钟线（分时图），按时间升序返回，并带回前收盘（prec，可能为 0）。
// count<=0 默认 241（一天 240 根 + 余量，保证整日分时不被截断）。
func (t *TencentAdapter) GetMin1Bars(ctx context.Context, market, symbol string, count int) ([]Min5Bar, float64, error) {
	if count <= 0 {
		count = 241
	}
	return t.getMinuteBars(ctx, market, symbol, minutePeriodM1, count)
}

// getMinuteBars m1/m5 共用取数路径。返回（升序根、前收盘、错误）。
func (t *TencentAdapter) getMinuteBars(ctx context.Context, market, symbol, period string, count int) ([]Min5Bar, float64, error) {
	if market != "cn" {
		return nil, 0, ErrNotSupported
	}
	code, ok := sinaCNSymbol(symbol) // 腾讯与新浪同为 sh/sz 前缀
	if !ok {
		return nil, 0, ErrSymbolInvalid
	}
	if count <= 0 {
		count = 60
	}
	if count > min5MaxCount {
		count = min5MaxCount
	}
	url := fmt.Sprintf("https://ifzq.gtimg.cn/appstock/app/kline/mkline?param=%s,%s,,%d", code, period, count)
	raw, status, err := doGet(ctx, url, map[string]string{"Referer": "https://gu.qq.com/"})
	if err != nil {
		return nil, 0, fmt.Errorf("%w: %v", ErrUpstream, err)
	}
	if status != http.StatusOK {
		return nil, 0, fmt.Errorf("%w: http %d", ErrUpstream, status)
	}
	return parseMinuteResponse(raw, code, period)
}

// parseMin5Response 解析 m5 响应（保留原签名，既有 fixture 单测与调用方零改动）。
func parseMin5Response(raw []byte, code string) ([]Min5Bar, error) {
	bars, _, err := parseMinuteResponse(raw, code, minutePeriodM5)
	return bars, err
}

// parseMinuteResponse 解析 mkline 响应（独立函数便于 fixture 单测）。
// period 决定取 data.{code}.m1 还是 .m5；两者行结构逐列同构。
func parseMinuteResponse(raw []byte, code, period string) ([]Min5Bar, float64, error) {
	var resp struct {
		Code int                        `json:"code"`
		Msg  string                     `json:"msg"`
		Data map[string]json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, 0, fmt.Errorf("%w: mkline 响应解析失败: %v", ErrUpstream, err)
	}
	if resp.Code != 0 {
		return nil, 0, fmt.Errorf("%w: mkline code=%d msg=%s", ErrUpstream, resp.Code, resp.Msg)
	}
	entry, ok := resp.Data[code]
	if !ok {
		return nil, 0, ErrNoData
	}
	var body struct {
		M1   [][]any `json:"m1"`
		M5   [][]any `json:"m5"`
		Prec any     `json:"prec"`
	}
	if err := json.Unmarshal(entry, &body); err != nil {
		return nil, 0, fmt.Errorf("%w: mkline %s 解析失败: %v", ErrUpstream, period, err)
	}
	rows := body.M5
	if period == minutePeriodM1 {
		rows = body.M1
	}
	if len(rows) == 0 {
		return nil, 0, ErrNoData // 非法代码/无数据：qt 空数组且无 m1/m5 键
	}
	prec := min5Atof(body.Prec)
	out := make([]Min5Bar, 0, len(rows))
	for _, row := range rows {
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
		return nil, 0, ErrNoData
	}
	return out, prec, nil
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
