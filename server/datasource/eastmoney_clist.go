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

// 东财 clist 全市场快照（M1 全市场日线的每日增量源）。
// 覆盖范围 = 沪深 A 股（深主板 m:0+t:6 / 创业板 m:0+t:80 / 沪主板 m:1+t:2 / 科创板 m:1+t:23），
// 不含北交所（cnSecid 不识别、日线拉不了、推荐链路也排除，见 DEVELOPMENT_PLAN M1）。
// 上游把 pz 硬钳制在 100（2026-07-08 实测：pz=6000 只回 100 行），必须按页翻：
// 约 5500 只 ≈ 56 页；fid=f12 按代码升序保证翻页稳定（涨跌幅序盘中会漂移导致重复/漏行）。
const (
	clistSpotFS     = "m:0+t:6,m:0+t:80,m:1+t:2,m:1+t:23"
	clistSpotFields = "f12,f14,f2,f3,f5,f6,f8,f15,f16,f17,f18,f124"
	clistPageSize   = 100
	clistMaxPages   = 80 // 5535/100=56 页，留余量；防上游 total 异常时翻页失控
	clistPageGap    = 200 * time.Millisecond
	clistUT         = "bd1d9ddb04089700cf9c27f6f7426281" // 东财网页公共 ut（社区惯例硬编码）
)

// GetCNSpotSnapshot 拉取沪深 A 股全市场行情快照（翻页聚合）。
// 走 e.get（push2 族断路器：限流自动降级 push2delay / 连续限流熔断）。
// 单页失败重试一次仍失败则整轮失败——部分快照落库会留下"当日只有一半股票有 bar"的
// 静默缺口，宁可整轮失败由调用方重试。调用方须自带充足的 ctx 预算（56 页约 1~2 分钟）。
func (e *EastMoneyAdapter) GetCNSpotSnapshot(ctx context.Context) ([]SpotRow, error) {
	all := make([]SpotRow, 0, 6000)
	total := 0
	for pn := 1; pn <= clistMaxPages; pn++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		rows, pageTotal, err := e.fetchSpotPage(ctx, pn)
		if err != nil {
			// 页级重试一次（doGet 内部已各重试一次，这里是整页级别的第二道）。
			time.Sleep(500 * time.Millisecond)
			rows, pageTotal, err = e.fetchSpotPage(ctx, pn)
			if err != nil {
				return nil, fmt.Errorf("全市场快照第 %d 页失败: %w", pn, err)
			}
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
		time.Sleep(clistPageGap)
	}
	if len(all) == 0 {
		return nil, ErrNoData
	}
	// 全市场快照的完整性硬校验：拿到的行数远小于 total 说明翻页中断/上游异常，
	// 这种半截快照对"全市场"语义是毒数据（增量同步会漏掉几千只），拒绝返回。
	if total > 0 && len(all) < total*9/10 {
		return nil, fmt.Errorf("%w: 全市场快照不完整 %d/%d", ErrUpstream, len(all), total)
	}
	return all, nil
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
		F15  json.RawMessage `json:"f15"`
		F16  json.RawMessage `json:"f16"`
		F17  json.RawMessage `json:"f17"`
		F18  json.RawMessage `json:"f18"`
		F124 json.RawMessage `json:"f124"`
	}
	var items []emSpot
	raw := parsed.Data.Diff
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
			if err != nil {
				continue
			}
			keys = append(keys, n)
			idx[n] = k
		}
		sort.Ints(keys)
		for _, n := range keys {
			items = append(items, m[idx[n]])
		}
	}
	rows := make([]SpotRow, 0, len(items))
	for _, it := range items {
		sym := strings.TrimSpace(it.F12)
		if sym == "" {
			continue
		}
		price, _ := emNum(it.F2)
		changePct, _ := emNum(it.F3)
		vol, _ := emNum(it.F5)
		amount, _ := emNum(it.F6)
		turnover, _ := emNum(it.F8)
		high, _ := emNum(it.F15)
		low, _ := emNum(it.F16)
		open, _ := emNum(it.F17)
		prevClose, _ := emNum(it.F18)
		ts, _ := emNum(it.F124)
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
			DataTime:     int64(ts),
		})
	}
	return rows, parsed.Data.Total, nil
}
