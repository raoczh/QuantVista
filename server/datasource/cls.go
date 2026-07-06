package datasource

import (
	"context"
	"crypto/md5"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// 财联社电报（telegraph）采集客户端。
// 上次实测（2026-07）：老接口 nodeapi/updateTelegraphList 已死，
// 只有 /v1/roll/get_roll_list 活着，需带 sign = md5hex(sha1hex(querystring))，
// querystring 按参数名字典序拼接（app/category/last_time/os/refresh_type/rn/sv 恰好有序）。
// 自带 stock_list 股票关联与 level 重要级（A/B 视为重要），免 token。

// ClsNewsItem 财联社电报条目（标准化输出）。
type ClsNewsItem struct {
	SourceID    string
	Title       string
	Content     string
	Brief       string
	PublishTime time.Time
	Important   bool
	Symbols     []string // 已归一为本项目 6 位代码
	URL         string
}

type clsRollResp struct {
	Errno int    `json:"errno"`
	Msg   string `json:"msg"`
	Data  struct {
		RollData []struct {
			ID      int64  `json:"id"`
			Title   string `json:"title"`
			Brief   string `json:"brief"`
			Content string `json:"content"`
			Ctime   int64  `json:"ctime"` // 秒级时间戳
			Level   string `json:"level"` // A/B/C，A/B 重要
			IsAd    int    `json:"is_ad"`
			ShareURL string `json:"shareurl"`
			StockList []struct {
				StockID string `json:"StockID"` // 形如 sh603979 / sz000001
				Name    string `json:"name"`
			} `json:"stock_list"`
		} `json:"roll_data"`
	} `json:"data"`
}

// clsSign 财联社签名：md5hex(sha1hex(qs))。
func clsSign(qs string) string {
	s1 := sha1.Sum([]byte(qs))
	h1 := hex.EncodeToString(s1[:])
	s2 := md5.Sum([]byte(h1))
	return hex.EncodeToString(s2[:])
}

// normalizeClsStockID 把财联社 sh603979/sz000001 归一为本项目 6 位代码。
// 非 A 股口径（前缀不识别或长度不符）丢弃。
func normalizeClsStockID(id string) (string, bool) {
	id = strings.ToLower(strings.TrimSpace(id))
	if len(id) != 8 {
		return "", false
	}
	if p := id[:2]; p != "sh" && p != "sz" {
		return "", false
	}
	code := id[2:]
	for _, ch := range code {
		if ch < '0' || ch > '9' {
			return "", false
		}
	}
	return code, true
}

// GetClsTelegraph 拉取财联社电报最新一批（rn 条，last_time 传 0 表示当前时刻往前）。
func GetClsTelegraph(ctx context.Context, lastTime int64, rn int) ([]ClsNewsItem, error) {
	if rn <= 0 || rn > 100 {
		rn = 20
	}
	if lastTime <= 0 {
		lastTime = time.Now().Unix()
	}
	qs := fmt.Sprintf("app=CailianpressWeb&category=&last_time=%d&os=web&refresh_type=1&rn=%d&sv=8.7.9", lastTime, rn)
	url := "https://www.cls.cn/v1/roll/get_roll_list?" + qs + "&sign=" + clsSign(qs)
	body, status, err := doGet(ctx, url, map[string]string{
		"Accept":  "application/json, text/plain, */*",
		"Referer": "https://www.cls.cn/telegraph",
	})
	if err != nil {
		return nil, err
	}
	if status != 200 {
		return nil, fmt.Errorf("cls telegraph HTTP %d", status)
	}
	var resp clsRollResp
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("cls telegraph 解析失败: %w", err)
	}
	if resp.Errno != 0 {
		return nil, fmt.Errorf("cls telegraph errno=%d msg=%s", resp.Errno, resp.Msg)
	}
	items := make([]ClsNewsItem, 0, len(resp.Data.RollData))
	for _, r := range resp.Data.RollData {
		if r.IsAd != 0 {
			continue
		}
		var syms []string
		for _, st := range r.StockList {
			if code, ok := normalizeClsStockID(st.StockID); ok {
				syms = append(syms, code)
			}
		}
		title := strings.TrimSpace(r.Title)
		if title == "" {
			title = strings.TrimSpace(r.Brief)
		}
		items = append(items, ClsNewsItem{
			SourceID:    strconv.FormatInt(r.ID, 10),
			Title:       title,
			Content:     strings.TrimSpace(r.Content),
			Brief:       strings.TrimSpace(r.Brief),
			PublishTime: time.Unix(r.Ctime, 0),
			Important:   r.Level == "A" || r.Level == "B",
			Symbols:     syms,
			URL:         strings.TrimSpace(r.ShareURL),
		})
	}
	return items, nil
}
