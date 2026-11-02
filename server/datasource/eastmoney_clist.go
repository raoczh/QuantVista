package datasource

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// 东财 clist 全市场快照（M1 全市场日线的每日增量源）。
// 覆盖范围 = 沪深 A 股（深主板 m:0+t:6 / 创业板 m:0+t:80 / 沪主板 m:1+t:2 / 科创板 m:1+t:23），
// 不含北交所（cnSecid 不识别、日线拉不了、推荐链路也排除，见 DEVELOPMENT_PLAN M1）。
// 上游把 pz 硬钳制在 100（2026-07-08 实测：pz=6000 只回 100 行），必须按页翻：
// 约 5500 只 ≈ 56 页；fid=f12 按代码升序保证翻页稳定（涨跌幅序盘中会漂移导致重复/漏行）。
// 估值字段（P3b 板块估值聚合，2026-07-10 实测锚定）：f9=市盈率(动) / f115=市盈率(TTM) /
// f23=市净率 / f100=东财行业板块名（与 m:90+t:2 行业板块 f14 名精确匹配，抽样 110/110；
// 退市壳票 f100 与估值字段全为 "-"，emNum 容错为 0，聚合侧按 >0 过滤自然排除）。
const (
	clistSpotFS     = "m:0+t:6,m:0+t:80,m:1+t:2,m:1+t:23"
	clistSpotFields = "f12,f14,f2,f3,f5,f6,f8,f9,f15,f16,f17,f18,f23,f100,f115,f124"
	clistPageSize   = 100
	clistMaxPages   = 80 // 5535/100=56 页，留余量；防上游 total 异常时翻页失控
	clistPageGap    = 200 * time.Millisecond
	clistUT         = "bd1d9ddb04089700cf9c27f6f7426281" // 东财网页公共 ut（社区惯例硬编码）
)

// GetCNSpotSnapshot 拉取沪深 A 股全市场行情快照（翻页聚合）。
// 走 e.get（push2 族断路器：限流自动降级 push2delay / 连续限流熔断）。
// HTTP 层有限重试后单页仍失败则整轮失败，不再额外重试整页；部分快照落库会留下
// 静默缺口。调用方须自带充足的分页预算，Manager 对全市场任务使用单独的长预算。
func (e *EastMoneyAdapter) GetCNSpotSnapshot(ctx context.Context) ([]SpotRow, error) {
	all := make([]SpotRow, 0, 6000)
	expectedTotal := 0
	seenSymbols := make(map[string]struct{}, 6000)
	for pn := 1; pn <= clistMaxPages; pn++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		rows, pageTotal, err := e.fetchSpotPage(ctx, pn)
		if err != nil {
			if canceled := ctx.Err(); canceled != nil {
				return nil, canceled
			}
			return nil, fmt.Errorf("全市场快照第 %d 页失败: %w", pn, err)
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if pn == 1 && pageTotal == 0 && len(rows) == 0 {
			return nil, ErrNoData
		}
		if pageTotal <= 0 || pageTotal > clistPageSize*clistMaxPages {
			return nil, fmt.Errorf("%w: 全市场快照 total 无效或超出分页上限: %d", ErrUpstream, pageTotal)
		}
		if pn == 1 {
			expectedTotal = pageTotal
		} else if pageTotal != expectedTotal {
			return nil, fmt.Errorf("%w: 全市场快照 total 翻页时变化: %d -> %d", ErrUpstream, expectedTotal, pageTotal)
		}
		expectedPageRows := min(clistPageSize, expectedTotal-len(all))
		if len(rows) != expectedPageRows {
			return nil, fmt.Errorf("%w: 全市场快照第 %d 页不完整: %d/%d", ErrUpstream, pn, len(rows), expectedPageRows)
		}
		for _, row := range rows {
			if !isSixDigitSymbol(row.Symbol) {
				return nil, fmt.Errorf("%w: 全市场快照第 %d 页代码无效", ErrUpstream, pn)
			}
			if _, exists := seenSymbols[row.Symbol]; exists {
				return nil, fmt.Errorf("%w: 全市场快照第 %d 页重复代码 %s", ErrUpstream, pn, row.Symbol)
			}
			seenSymbols[row.Symbol] = struct{}{}
			all = append(all, row)
		}
		if len(seenSymbols) == expectedTotal {
			return all, nil
		}
		if err := waitSpotSnapshotInterval(ctx, clistPageGap); err != nil {
			return nil, err
		}
	}
	return nil, fmt.Errorf("%w: 全市场快照不完整 %d/%d", ErrUpstream, len(seenSymbols), expectedTotal)
}

// 快照的翻页和重试等待必须可取消，取消后不再向上游发起请求。
func waitSpotSnapshotInterval(ctx context.Context, interval time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	timer := time.NewTimer(interval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return ctx.Err()
	}
}

// fetchSpotPage 拉取一页并解析。返回 (rows, total, err)。
func (e *EastMoneyAdapter) fetchSpotPage(ctx context.Context, pn int) ([]SpotRow, int, error) {
	url := fmt.Sprintf(
		"https://%d.push2.eastmoney.com/api/qt/clist/get?pn=%d&pz=%d&po=0&np=1&fid=f12&fltt=2&invt=2&ut=%s&fs=%s&fields=%s",
		emNode(), pn, clistPageSize, clistUT, clistSpotFS, clistSpotFields,
	)
	body, status, err := e.get(ctx, url, map[string]string{"Referer": "https://quote.eastmoney.com/"})
	if err != nil {
		return nil, 0, fmt.Errorf("%w: %v", ErrUpstream, err)
	}
	if status != http.StatusOK {
		return nil, 0, fmt.Errorf("%w: http %d", ErrUpstream, status)
	}
	return parseClistSpot(body)
}

// parseClistSpot 解析 clist 快照一页（抽出便于单测，防上游字段漂移）。
// diff 兼容数组与 {"0":{...}} 对象两种形态（同 GetSectorRanking 前例）；
// 停牌行（f2 为 "-"）保留，价格字段解析为 0。
func parseClistSpot(body []byte) ([]SpotRow, int, error) {
	var parsed struct {
		Data struct {
			Total int             `json:"total"`
			Diff  json.RawMessage `json:"diff"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, 0, fmt.Errorf("%w: 解析失败 %v", ErrUpstream, err)
	}
	type emSpot struct {
		F12  string          `json:"f12"`
		F14  string          `json:"f14"`
		F2   json.RawMessage `json:"f2"`
		F3   json.RawMessage `json:"f3"`
		F5   json.RawMessage `json:"f5"`
		F6   json.RawMessage `json:"f6"`
		F8   json.RawMessage `json:"f8"`
		F9   json.RawMessage `json:"f9"`
		F15  json.RawMessage `json:"f15"`
		F16  json.RawMessage `json:"f16"`
		F17  json.RawMessage `json:"f17"`
		F18  json.RawMessage `json:"f18"`
		F23  json.RawMessage `json:"f23"`
		F100 string          `json:"f100"`
		F115 json.RawMessage `json:"f115"`
		F124 json.RawMessage `json:"f124"`
	}
	var items []emSpot
	raw := bytes.TrimSpace(parsed.Data.Diff)
	if len(raw) > 0 && raw[0] == '[' {
		if err := json.Unmarshal(raw, &items); err != nil {
			return nil, 0, fmt.Errorf("%w: 解析失败 %v", ErrUpstream, err)
		}
	} else if len(raw) > 0 && raw[0] == '{' {
		m := map[string]emSpot{}
		if err := json.Unmarshal(raw, &m); err != nil {
			return nil, 0, fmt.Errorf("%w: 解析失败 %v", ErrUpstream, err)
		}
		keys := make([]int, 0, len(m))
		idx := map[int]string{}
		for k := range m {
			n, err := strconv.Atoi(k)
			if err != nil || n < 0 || n >= len(m) || strconv.Itoa(n) != k {
				return nil, 0, fmt.Errorf("%w: 快照对象行号无效", ErrUpstream)
			}
			keys = append(keys, n)
			idx[n] = k
		}
		sort.Ints(keys)
		for _, n := range keys {
			items = append(items, m[idx[n]])
		}
	} else if len(raw) > 0 && string(raw) != "null" {
		return nil, 0, fmt.Errorf("%w: 快照 diff 不是数组或对象", ErrUpstream)
	}
	rows := make([]SpotRow, 0, len(items))
	for _, it := range items {
		sym := strings.TrimSpace(it.F12)
		if !isSixDigitSymbol(sym) {
			return nil, 0, fmt.Errorf("%w: 快照存在无效股票代码", ErrUpstream)
		}
		price, _ := emNum(it.F2)
		changePct, _ := emNum(it.F3)
		vol, _ := emNum(it.F5)
		amount, _ := emNum(it.F6)
		turnover, _ := emNum(it.F8)
		pe, _ := emNum(it.F9)
		high, _ := emNum(it.F15)
		low, _ := emNum(it.F16)
		open, _ := emNum(it.F17)
		prevClose, _ := emNum(it.F18)
		pb, _ := emNum(it.F23)
		peTTM, _ := emNum(it.F115)
		ts, _ := emNum(it.F124)
		industry := strings.TrimSpace(it.F100)
		if industry == "-" {
			industry = "" // 退市壳票行业名占位符归一为空（同价格字段容错语义）
		}
		rows = append(rows, SpotRow{
			Symbol:       sym,
			Name:         strings.TrimSpace(it.F14),
			Price:        price,
			ChangePct:    changePct,
			Open:         open,
			High:         high,
			Low:          low,
			PrevClose:    prevClose,
			Volume:       int64(vol),
			Amount:       amount,
			TurnoverRate: turnover,
			PE:           pe,
			PETTM:        peTTM,
			PB:           pb,
			Industry:     industry,
			DataTime:     int64(ts),
			Source:       "eastmoney",
		})
	}
	return rows, parsed.Data.Total, nil
}
