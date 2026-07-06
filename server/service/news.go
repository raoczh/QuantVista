package service

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"strings"
	"sync"
	"time"

	"quantvista/common"
	"quantvista/datasource"
	"quantvista/model"

	"gorm.io/gorm/clause"
)

// NewsService 新闻采集与查询（N1 新闻舆情地基）。
//
// 轻量去重三层（不抄 StockAgent 拉全量逐条比对）：
//  1. DB content_hash 唯一索引 + INSERT IGNORE 语义（最终兜底）；
//  2. 进程内 source:source_id 与 title_hash 缓存（1 万上限砍半，防重复打 DB）；
//  3. 标题相似度：归一化后 bigram Dice ≥ 0.85，比对最近 72h 内存标题池
//     （拦"同一事件多源措辞微调"的跨源重复）。
type NewsService struct {
	mu     sync.Mutex
	seen   map[string]struct{} // "src:id" 与 "t:"+titleHash
	titles []titleEntry        // 最近 72h 标题池（相似度比对）
}

type titleEntry struct {
	norm string
	at   time.Time
}

const (
	newsSeenCap       = 10000
	newsTitleWindow   = 72 * time.Hour
	newsDiceThreshold = 0.85
	newsContentMax    = 3000 // 正文截断（字符）

	newsSourceCls = "cls"
	newsSourceEM  = "eastmoney"

	newsCategoryTelegraph = "telegraph"
	newsCategoryFlash     = "flash"
	newsCategoryStock     = "stock"

	optNewsCursorCls = "news_cursor_cls"
	optNewsCursorEM  = "news_cursor_em"
)

// newsTTLDays 保留档位（抄 collector.yaml lifecycle 思路为常量）：
// 快讯类 7 天、个股新闻 60 天；ImportantMark（政策级/重磅）统一 90 天。
var newsTTLDays = map[string]int{
	newsCategoryTelegraph: 7,
	newsCategoryFlash:     7,
	newsCategoryStock:     60,
}

const newsImportantTTLDays = 90

func NewNewsService() *NewsService {
	return &NewsService{seen: make(map[string]struct{})}
}

// --- 去重工具（纯函数，单测覆盖） ---

// newsContentHash 去重指纹：MD5(标题 + 正文前 500 字)。
func newsContentHash(title, content string) string {
	r := []rune(content)
	if len(r) > 500 {
		r = r[:500]
	}
	sum := md5.Sum([]byte(title + string(r)))
	return hex.EncodeToString(sum[:])
}

// normalizeNewsTitle 标题归一：去空白与常见标点、转小写，只留字母数字汉字。
func normalizeNewsTitle(s string) string {
	var b strings.Builder
	for _, ch := range strings.ToLower(s) {
		switch {
		case ch >= 'a' && ch <= 'z', ch >= '0' && ch <= '9', ch >= 0x4e00 && ch <= 0x9fff:
			b.WriteRune(ch)
		}
	}
	return b.String()
}

// bigramDice 归一化标题的 bigram Dice 系数（0~1，1 完全相同）。
func bigramDice(a, b string) float64 {
	ra, rb := []rune(a), []rune(b)
	if len(ra) < 2 || len(rb) < 2 {
		if a == b && a != "" {
			return 1
		}
		return 0
	}
	ga := make(map[string]int, len(ra))
	for i := 0; i+1 < len(ra); i++ {
		ga[string(ra[i:i+2])]++
	}
	inter := 0
	for i := 0; i+1 < len(rb); i++ {
		g := string(rb[i : i+2])
		if ga[g] > 0 {
			ga[g]--
			inter++
		}
	}
	return 2 * float64(inter) / float64(len(ra)-1+len(rb)-1)
}

// dedupeCheck 进程内两层去重：source:id / title_hash 缓存 + 72h 标题相似度。
// 返回 true 表示重复应跳过；不重复则登记进缓存。调用方持有锁责任在本函数内。
func (s *NewsService) dedupeCheck(source, sourceID, title string, at time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	idKey := source + ":" + sourceID
	norm := normalizeNewsTitle(title)
	titleKey := "t:" + norm
	if _, ok := s.seen[idKey]; ok {
		return true
	}
	if norm != "" {
		if _, ok := s.seen[titleKey]; ok {
			return true
		}
	}

	// 标题池剪枝（过期项顺带清理）+ 相似度比对。
	cutoff := time.Now().Add(-newsTitleWindow)
	kept := s.titles[:0]
	dup := false
	for _, e := range s.titles {
		if e.at.Before(cutoff) {
			continue
		}
		kept = append(kept, e)
		if !dup && norm != "" && bigramDice(norm, e.norm) >= newsDiceThreshold {
			dup = true
		}
	}
	s.titles = kept
	if dup {
		return true
	}

	// 登记。缓存超限砍半（map 迭代序随机，等价随机淘汰）。
	if len(s.seen) >= newsSeenCap {
		drop := len(s.seen) / 2
		for k := range s.seen {
			if drop == 0 {
				break
			}
			delete(s.seen, k)
			drop--
		}
	}
	s.seen[idKey] = struct{}{}
	if norm != "" {
		s.seen[titleKey] = struct{}{}
		s.titles = append(s.titles, titleEntry{norm: norm, at: at})
	}
	return false
}

// insertNews 落库（DB 唯一索引兜底：冲突静默忽略）。返回是否新插入。
func insertNews(n *model.News) bool {
	if common.DB == nil {
		return false
	}
	res := common.DB.Clauses(clause.OnConflict{DoNothing: true}).Create(n)
	if res.Error != nil {
		common.SysWarn("新闻入库失败: %v", res.Error)
		return false
	}
	return res.RowsAffected > 0
}

func marshalSymbols(syms []string) string {
	if len(syms) == 0 {
		return ""
	}
	b, err := json.Marshal(syms)
	if err != nil {
		return ""
	}
	return string(b)
}

// --- 采集 ---

// collectCls 财联社电报一轮。游标=最近一条 ctime（落库防重启重采），
// 带 5 分钟重叠窗防同秒漏采，重叠部分由去重层吸收。
func (s *NewsService) collectCls(ctx context.Context) (inserted int) {
	cursor := readNewsCursor(optNewsCursorCls)
	items, err := datasource.GetClsTelegraph(ctx, 0, 30)
	if err != nil {
		common.SysDebug("财联社电报采集跳过: %v", err)
		return 0
	}
	maxTs := cursor
	for _, it := range items {
		ts := it.PublishTime.Unix()
		if ts > maxTs {
			maxTs = ts
		}
		if cursor > 0 && ts < cursor-300 {
			continue
		}
		if s.dedupeCheck(newsSourceCls, it.SourceID, it.Title, it.PublishTime) {
			continue
		}
		content := []rune(it.Content)
		if len(content) > newsContentMax {
			content = content[:newsContentMax]
		}
		n := &model.News{
			Title: it.Title, Content: string(content), Summary: it.Brief,
			URL: it.URL, Source: newsSourceCls, SourceID: it.SourceID,
			Category: newsCategoryTelegraph, PublishTime: it.PublishTime,
			CollectTime: time.Now(), RelatedSymbols: marshalSymbols(it.Symbols),
			SourcePriority: 1, ContentHash: newsContentHash(it.Title, it.Content),
			ImportantMark: it.Important,
		}
		if insertNews(n) {
			inserted++
		}
	}
	if maxTs > cursor {
		writeNewsCursor(optNewsCursorCls, maxTs)
	}
	return inserted
}

// collectEMFast 东财 7×24 快讯一轮（只取第一页，增量靠轮询+去重）。
func (s *NewsService) collectEMFast(ctx context.Context) (inserted int) {
	cursor := readNewsCursor(optNewsCursorEM)
	items, _, err := datasource.GetEMFastNews(ctx, "", 20)
	if err != nil {
		common.SysDebug("东财快讯采集跳过: %v", err)
		return 0
	}
	maxTs := cursor
	for _, it := range items {
		ts := it.PublishTime.Unix()
		if ts > maxTs {
			maxTs = ts
		}
		if cursor > 0 && ts < cursor-300 {
			continue
		}
		if s.dedupeCheck(newsSourceEM, it.SourceID, it.Title, it.PublishTime) {
			continue
		}
		n := &model.News{
			Title: it.Title, Content: it.Summary, Summary: it.Summary,
			URL: it.URL, Source: newsSourceEM, SourceID: it.SourceID,
			Category: newsCategoryFlash, PublishTime: it.PublishTime,
			CollectTime: time.Now(), RelatedSymbols: marshalSymbols(it.Symbols),
			SourcePriority: 2, ContentHash: newsContentHash(it.Title, it.Summary),
		}
		if insertNews(n) {
			inserted++
		}
	}
	if maxTs > cursor {
		writeNewsCursor(optNewsCursorEM, maxTs)
	}
	return inserted
}

// collectStockNews 个股新闻：自选∪持仓 distinct 标的（上限 50），逐只搜索。
// search-api 有 TLS 指纹风险：单只失败即整轮放弃（本轮降级，不反复烧）。
func (s *NewsService) collectStockNews(ctx context.Context) (inserted int) {
	if common.DB == nil {
		return 0
	}
	var syms []string
	common.DB.Raw(`SELECT DISTINCT symbol FROM (
		SELECT symbol FROM watchlist_items UNION SELECT symbol FROM positions
	) t LIMIT 50`).Scan(&syms)
	for _, sym := range syms {
		if len(sym) != 6 { // 只做 A 股 6 位代码口径
			continue
		}
		items, err := datasource.GetEMStockNews(ctx, sym, 10)
		if err != nil {
			common.SysDebug("个股新闻采集降级（本轮放弃）: %v", err)
			return inserted
		}
		for _, it := range items {
			if s.dedupeCheck(newsSourceEM, "stock:"+it.SourceID, it.Title, it.PublishTime) {
				continue
			}
			n := &model.News{
				Title: it.Title, Content: it.Summary, Summary: it.Summary,
				URL: it.URL, Source: newsSourceEM, SourceID: it.SourceID,
				Category: newsCategoryStock, PublishTime: it.PublishTime,
				CollectTime: time.Now(), RelatedSymbols: marshalSymbols(it.Symbols),
				SourcePriority: 3, ContentHash: newsContentHash(it.Title, it.Summary),
			}
			if insertNews(n) {
				inserted++
			}
		}
		select {
		case <-ctx.Done():
			return inserted
		case <-time.After(300 * time.Millisecond): // 节流，敬畏免费源
		}
	}
	return inserted
}

func readNewsCursor(key string) int64 {
	if common.DB == nil {
		return 0
	}
	var opt model.Option
	if err := common.DB.Where("`key` = ?", key).First(&opt).Error; err != nil {
		return 0
	}
	v, _ := strconv.ParseInt(opt.Value, 10, 64)
	return v
}

func writeNewsCursor(key string, v int64) {
	if err := model.UpsertOption(key, strconv.FormatInt(v, 10)); err != nil {
		common.SysWarn("写新闻游标失败: %v", err)
	}
}

// CleanupExpired TTL 清理：按 category 档位删过期，重要标记统一 90 天。
func (s *NewsService) CleanupExpired() {
	if common.DB == nil {
		return
	}
	now := time.Now()
	for cat, days := range newsTTLDays {
		res := common.DB.Where("category = ? AND important_mark = ? AND publish_time < ?",
			cat, false, now.AddDate(0, 0, -days)).Delete(&model.News{})
		if res.Error != nil {
			common.SysWarn("新闻 TTL 清理失败(%s): %v", cat, res.Error)
		} else if res.RowsAffected > 0 {
			common.SysLog("新闻 TTL 清理(%s): 删除 %d 条", cat, res.RowsAffected)
		}
	}
	if res := common.DB.Where("important_mark = ? AND publish_time < ?",
		true, now.AddDate(0, 0, -newsImportantTTLDays)).Delete(&model.News{}); res.Error != nil {
		common.SysWarn("新闻 TTL 清理失败(important): %v", res.Error)
	}
}

// --- 查询 ---

// ListNews 新闻查询：可选 symbol（RelatedSymbols JSON LIKE 匹配）、source、limit。
// 列表按发布时间倒序；正文大字段列表页不需要，排除以省流量。
func (s *NewsService) ListNews(symbol, source string, limit int) ([]model.News, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	q := common.DB.Model(&model.News{}).
		Select("id, title, summary, url, source, category, publish_time, related_symbols, source_priority, sentiment, sentiment_score, important_mark").
		Order("publish_time DESC").Limit(limit)
	if source != "" {
		q = q.Where("source = ?", source)
	}
	if symbol = strings.TrimSpace(symbol); symbol != "" {
		q = q.Where("related_symbols LIKE ?", "%\""+symbol+"\"%")
	}
	var rows []model.News
	if err := q.Find(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}

// StartNewsJobs 新闻后台任务：
//   - 快讯类（财联社电报 + 东财 7×24）每 5 分钟一轮，启动即跑一次；
//   - 个股新闻每 60 分钟一轮（TLS 指纹风险源，失败整轮降级）；
//   - 每日 03:10 TTL 清理。
func StartNewsJobs() *NewsService {
	svc := NewNewsService()

	go func() {
		round := func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			n1 := svc.collectCls(ctx)
			n2 := svc.collectEMFast(ctx)
			if n1+n2 > 0 {
				common.SysLog("快讯采集入库: 财联社 %d 东财 %d", n1, n2)
			}
		}
		round()
		t := time.NewTicker(5 * time.Minute)
		defer t.Stop()
		for range t.C {
			round()
		}
	}()

	go func() {
		t := time.NewTicker(60 * time.Minute)
		defer t.Stop()
		for range t.C {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			if n := svc.collectStockNews(ctx); n > 0 {
				common.SysLog("个股新闻采集入库: %d", n)
			}
			cancel()
		}
	}()

	go func() {
		for {
			now := time.Now()
			next := time.Date(now.Year(), now.Month(), now.Day(), 3, 10, 0, 0, now.Location())
			if !next.After(now) {
				next = next.Add(24 * time.Hour)
			}
			time.Sleep(time.Until(next))
			svc.CleanupExpired()
		}
	}()

	return svc
}
