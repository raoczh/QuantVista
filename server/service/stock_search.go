package service

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"quantvista/common"
	"quantvista/model"

	"github.com/mozillazg/go-pinyin"
)

const (
	StockSearchDefaultLimit = 20
	StockSearchMaxLimit     = 50
	stockSearchMaxQueryLen  = 64
)

const (
	stockSearchSourceUniverse = "stock_universe_daily"
	stockSearchSourceFallback = "market_sync_state"
)

// StockSearchItem 是全局搜索的轻量结果，不包含实时行情。
type StockSearchItem struct {
	Symbol      string `json:"symbol"`
	Market      string `json:"market"`
	Name        string `json:"name"`
	Industry    string `json:"industry,omitempty"`
	AsOf        string `json:"as_of"`
	InWatchlist bool   `json:"in_watchlist"`
	HasPosition bool   `json:"has_position"`
}

// StockSearchResult 显式说明本次搜索使用的数据源和统一快照日期。
// 降级源不是逐日快照，Items 中的 as_of 会保留每只股票自己的最近日线日期。
type StockSearchResult struct {
	Query  string            `json:"query"`
	Source string            `json:"source"`
	AsOf   string            `json:"as_of"`
	Items  []StockSearchItem `json:"items"`
}

// StockSearchService 只查询本地数据库，不持有行情服务或外部数据源。
type StockSearchService struct{}

func NewStockSearchService() *StockSearchService { return &StockSearchService{} }

type stockSearchCandidate struct {
	StockSearchItem
	fullPinyin []string
	initials   []string
	rank       int
}

// Search 按名称、代码、全拼和首字母搜索股票，并批量富化当前用户关系。
func (s *StockSearchService) Search(ctx context.Context, userID int64, query string, limit int) (*StockSearchResult, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, errors.New("搜索关键词不能为空")
	}
	if !utf8.ValidString(query) || utf8.RuneCountInString(query) > stockSearchMaxQueryLen {
		return nil, errors.New("搜索关键词过长（最多 64 个字符）")
	}
	if limit < 1 || limit > StockSearchMaxLimit {
		return nil, errors.New("limit 须在 1 到 50 之间")
	}
	if common.DB == nil {
		return nil, errors.New("数据库不可用")
	}

	result := &StockSearchResult{Query: query, Items: []StockSearchItem{}}
	candidates, source, asOf, err := loadStockSearchCandidates(ctx, query)
	if err != nil {
		return nil, err
	}
	result.Source, result.AsOf = source, asOf
	if len(candidates) == 0 {
		return result, nil
	}

	needle := strings.ToLower(query)
	pinyinNeedle := compactSearchText(query)
	matched := make([]stockSearchCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		rank, ok := stockSearchRank(candidate, needle, pinyinNeedle)
		if !ok {
			continue
		}
		candidate.rank = rank
		matched = append(matched, candidate)
	}
	sort.Slice(matched, func(i, j int) bool {
		if matched[i].rank != matched[j].rank {
			return matched[i].rank < matched[j].rank
		}
		left, right := strings.ToLower(matched[i].Symbol), strings.ToLower(matched[j].Symbol)
		if left != right {
			return left < right
		}
		return strings.ToLower(matched[i].Market) < strings.ToLower(matched[j].Market)
	})
	if len(matched) > limit {
		matched = matched[:limit]
	}

	items := make([]StockSearchItem, len(matched))
	for i := range matched {
		items[i] = matched[i].StockSearchItem
	}
	if err := enrichStockSearchRelations(ctx, userID, items); err != nil {
		return nil, err
	}
	result.Items = items
	return result, nil
}

func loadStockSearchCandidates(ctx context.Context, query string) ([]stockSearchCandidate, string, string, error) {
	db := common.DB.WithContext(ctx)
	var latestDate string
	if err := db.Model(&model.StockUniverseDaily{}).
		Select("COALESCE(MAX(trade_date), '')").Scan(&latestDate).Error; err != nil {
		return nil, "", "", err
	}

	withPinyin := needsPinyinScan(query)
	if latestDate != "" {
		q := db.Select("symbol", "market", "name", "industry").
			Where("trade_date = ?", latestDate)
		if !withPinyin {
			pattern := "%" + escapeLikePattern(strings.ToLower(query)) + "%"
			q = q.Where("LOWER(symbol) LIKE ? ESCAPE '!' OR LOWER(name) LIKE ? ESCAPE '!'", pattern, pattern)
		}
		var rows []model.StockUniverseDaily
		if err := q.Find(&rows).Error; err != nil {
			return nil, "", "", err
		}
		out := make([]stockSearchCandidate, 0, len(rows))
		seen := make(map[string]struct{}, len(rows))
		for _, row := range rows {
			key := stockSearchKey(row.Market, row.Symbol)
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			candidate := stockSearchCandidate{StockSearchItem: StockSearchItem{
				Symbol: row.Symbol, Market: row.Market, Name: row.Name,
				Industry: row.Industry, AsOf: latestDate,
			}}
			if withPinyin {
				candidate.fullPinyin, candidate.initials = stockNamePinyin(row.Name)
			}
			out = append(out, candidate)
		}
		return out, stockSearchSourceUniverse, latestDate, nil
	}

	var fallbackMeta struct {
		Total int64  `gorm:"column:total"`
		AsOf  string `gorm:"column:as_of"`
	}
	if err := db.Model(&model.MarketSyncState{}).
		Select("COUNT(*) AS total, COALESCE(MAX(last_bar_date), '') AS as_of").
		Scan(&fallbackMeta).Error; err != nil {
		return nil, "", "", err
	}
	if fallbackMeta.Total == 0 {
		return []stockSearchCandidate{}, "", "", nil
	}
	var latestUpdated model.MarketSyncState
	if err := db.Select("updated_at").
		Where("last_bar_date = '' OR last_bar_date IS NULL").
		Order("updated_at DESC").
		Limit(1).
		Find(&latestUpdated).Error; err != nil {
		return nil, "", "", err
	}
	if !latestUpdated.UpdatedAt.IsZero() {
		if updatedDate := latestUpdated.UpdatedAt.Format("2006-01-02"); updatedDate > fallbackMeta.AsOf {
			fallbackMeta.AsOf = updatedDate
		}
	}

	q := db.Select("symbol", "market", "name", "last_bar_date", "updated_at")
	if !withPinyin {
		pattern := "%" + escapeLikePattern(strings.ToLower(query)) + "%"
		q = q.Where("LOWER(symbol) LIKE ? ESCAPE '!' OR LOWER(name) LIKE ? ESCAPE '!'", pattern, pattern)
	}
	var rows []model.MarketSyncState
	if err := q.Find(&rows).Error; err != nil {
		return nil, "", "", err
	}
	if len(rows) == 0 {
		return []stockSearchCandidate{}, stockSearchSourceFallback, fallbackMeta.AsOf, nil
	}

	out := make([]stockSearchCandidate, 0, len(rows))
	seen := make(map[string]struct{}, len(rows))
	for _, row := range rows {
		key := stockSearchKey(row.Market, row.Symbol)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		rowAsOf := row.LastBarDate
		if rowAsOf == "" && !row.UpdatedAt.IsZero() {
			rowAsOf = row.UpdatedAt.Format("2006-01-02")
		}
		candidate := stockSearchCandidate{StockSearchItem: StockSearchItem{
			Symbol: row.Symbol, Market: row.Market, Name: row.Name, AsOf: rowAsOf,
		}}
		if withPinyin {
			candidate.fullPinyin, candidate.initials = stockNamePinyin(row.Name)
		}
		out = append(out, candidate)
	}
	return out, stockSearchSourceFallback, fallbackMeta.AsOf, nil
}

func stockSearchRank(candidate stockSearchCandidate, needle, pinyinNeedle string) (int, bool) {
	symbol := strings.ToLower(candidate.Symbol)
	name := strings.ToLower(candidate.Name)
	switch {
	case symbol == needle:
		return 0, true
	case strings.HasPrefix(symbol, needle):
		return 1, true
	case name == needle:
		return 2, true
	case strings.HasPrefix(name, needle):
		return 3, true
	case pinyinNeedle != "" && anySearchPrefix(candidate.fullPinyin, pinyinNeedle):
		return 4, true
	case pinyinNeedle != "" && anySearchPrefix(candidate.initials, pinyinNeedle):
		return 5, true
	case strings.Contains(symbol, needle) || strings.Contains(name, needle) ||
		(pinyinNeedle != "" && (anySearchContains(candidate.fullPinyin, pinyinNeedle) ||
			anySearchContains(candidate.initials, pinyinNeedle))):
		return 6, true
	default:
		return 0, false
	}
}

func anySearchPrefix(values []string, needle string) bool {
	for _, value := range values {
		if strings.HasPrefix(value, needle) {
			return true
		}
	}
	return false
}

func anySearchContains(values []string, needle string) bool {
	for _, value := range values {
		if strings.Contains(value, needle) {
			return true
		}
	}
	return false
}

func enrichStockSearchRelations(ctx context.Context, userID int64, items []StockSearchItem) error {
	if userID <= 0 || len(items) == 0 {
		return nil
	}
	symbols := make([]string, 0, len(items))
	seenSymbols := make(map[string]struct{}, len(items))
	for _, item := range items {
		if _, ok := seenSymbols[item.Symbol]; ok {
			continue
		}
		seenSymbols[item.Symbol] = struct{}{}
		symbols = append(symbols, item.Symbol)
	}
	type stockRef struct {
		Symbol string
		Market string
	}
	var watched []stockRef
	if err := common.DB.WithContext(ctx).Model(&model.WatchlistItem{}).
		Select("symbol", "market").
		Where("user_id = ? AND symbol IN ?", userID, symbols).
		Find(&watched).Error; err != nil {
		return err
	}
	var held []stockRef
	if err := common.DB.WithContext(ctx).Model(&model.Position{}).
		Select("symbol", "market").
		Where("user_id = ? AND symbol IN ? AND status = ? AND quantity > 0",
			userID, symbols, model.PositionStatusHolding).
		Find(&held).Error; err != nil {
		return err
	}

	watchedSet := make(map[string]struct{}, len(watched))
	for _, ref := range watched {
		watchedSet[stockSearchKey(ref.Market, ref.Symbol)] = struct{}{}
	}
	heldSet := make(map[string]struct{}, len(held))
	for _, ref := range held {
		heldSet[stockSearchKey(ref.Market, ref.Symbol)] = struct{}{}
	}
	for i := range items {
		key := stockSearchKey(items[i].Market, items[i].Symbol)
		_, items[i].InWatchlist = watchedSet[key]
		_, items[i].HasPosition = heldSet[key]
	}
	return nil
}

func stockSearchKey(market, symbol string) string {
	return strings.ToLower(strings.TrimSpace(market)) + "\x00" + strings.ToLower(strings.TrimSpace(symbol))
}

func needsPinyinScan(query string) bool {
	for _, r := range query {
		if r <= unicode.MaxASCII && unicode.IsLetter(r) {
			return true
		}
	}
	return false
}

func escapeLikePattern(value string) string {
	value = strings.ReplaceAll(value, "!", "!!")
	value = strings.ReplaceAll(value, "%", "!%")
	return strings.ReplaceAll(value, "_", "!_")
}

func compactSearchText(value string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		return unicode.ToLower(r)
	}, value)
}

type stockPinyinKeys struct {
	full     []string
	initials []string
}

var stockPinyinCache sync.Map

func stockNamePinyin(name string) ([]string, []string) {
	if cached, ok := stockPinyinCache.Load(name); ok {
		keys := cached.(stockPinyinKeys)
		return keys.full, keys.initials
	}
	fallback := func(r rune, _ pinyin.Args) []string {
		if unicode.IsSpace(r) {
			return nil
		}
		return []string{strings.ToLower(string(r))}
	}
	keys := stockPinyinKeys{
		full:     stockPinyinVariants(name, pinyin.Normal, fallback),
		initials: stockPinyinVariants(name, pinyin.FirstLetter, fallback),
	}
	stockPinyinCache.Store(name, keys)
	return keys.full, keys.initials
}

const stockPinyinVariantLimit = 64

func stockPinyinVariants(name string, style int, fallback func(rune, pinyin.Args) []string) []string {
	args := pinyin.NewArgs()
	args.Style = style
	args.Heteronym = true
	args.Fallback = fallback
	parts := pinyin.Pinyin(name, args)
	variants := []string{""}
	for _, options := range parts {
		if len(options) == 0 {
			continue
		}
		next := make([]string, 0, min(len(variants)*len(options), stockPinyinVariantLimit))
		seen := make(map[string]struct{}, cap(next))
		for _, prefix := range variants {
			for _, option := range options {
				value := compactSearchText(prefix + option)
				if _, ok := seen[value]; ok {
					continue
				}
				seen[value] = struct{}{}
				next = append(next, value)
				if len(next) == stockPinyinVariantLimit {
					break
				}
			}
			if len(next) == stockPinyinVariantLimit {
				break
			}
		}
		variants = next
	}
	return variants
}
