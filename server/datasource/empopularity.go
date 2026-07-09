package datasource

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// 股吧人气榜（M3a）：emappdata getAllCurrentList，POST JSON，仅前 100 名。
// 独立域名（非 push2 族）、免鉴权（appId/globalId 为社区惯例硬编码值），不经断路器。
//
// 字段锚点（2026-07-08 实测）：sc="SZ000725"（市场前缀+代码）、rk=当前名次、
// hisRc=昨日名次——**负值（实测 -3，而非传闻的 -1）= 昨日不在榜（新上榜）**，
// 消费方统一按 hisRc<=0 判新上榜，不硬编码具体负值。
const popularityURL = "https://emappdata.eastmoney.com/stockrank/getAllCurrentList"

// PopularityRow 人气榜单行。
type PopularityRow struct {
	Symbol   string `json:"symbol"`
	Market   string `json:"market"`    // 恒 cn（sc 前缀 SH/SZ 已并入 symbol 语义）
	Rank     int    `json:"rank"`      // 当前名次 1~100
	PrevRank int    `json:"prev_rank"` // 昨日名次；<=0 = 昨日不在榜（新上榜）
}

// GetPopularityTop 拉取股吧人气榜前 100。
func GetPopularityTop(ctx context.Context) ([]PopularityRow, error) {
	payload := map[string]any{
		"appId":      "appId01",
		"globalId":   "786e4c21-70dc-435a-93bb-38", // 社区惯例固定值（akshare 同款），非用户标识
		"marketType": "",
		"pageNo":     1,
		"pageSize":   100,
	}
	body, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, popularityURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", browserUA)
	req.Header.Set("Content-Type", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUpstream, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: http %d", ErrUpstream, resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUpstream, err)
	}
	return parsePopularity(raw)
}

// parsePopularity 解析人气榜响应（抽出便于单测）。
func parsePopularity(raw []byte) ([]PopularityRow, error) {
	var parsed struct {
		Status int `json:"status"`
		Data   []struct {
			Sc    string `json:"sc"`
			Rk    int    `json:"rk"`
			HisRc int    `json:"hisRc"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("%w: 人气榜解析失败 %v", ErrUpstream, err)
	}
	if parsed.Status != 0 || len(parsed.Data) == 0 {
		return nil, ErrNoData
	}
	out := make([]PopularityRow, 0, len(parsed.Data))
	for _, it := range parsed.Data {
		sym := popularitySymbol(it.Sc)
		if sym == "" || it.Rk <= 0 {
			continue
		}
		out = append(out, PopularityRow{Symbol: sym, Market: "cn", Rank: it.Rk, PrevRank: it.HisRc})
	}
	if len(out) == 0 {
		return nil, ErrNoData
	}
	return out, nil
}

// popularitySymbol 剥离 sc 的市场前缀（SH600519/SZ000725 → 6 位代码），
// 校验可映射 secid（北交所 BJ 前缀等不支持的标的排除）。
func popularitySymbol(sc string) string {
	sc = strings.TrimSpace(strings.ToUpper(sc))
	if len(sc) != 8 || (!strings.HasPrefix(sc, "SH") && !strings.HasPrefix(sc, "SZ")) {
		return ""
	}
	sym := sc[2:]
	if _, ok := cnSecid(sym); !ok {
		return ""
	}
	return sym
}
