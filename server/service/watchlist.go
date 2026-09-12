package service

import (
	"context"
	"errors"
	"sort"
	"strings"
	"time"

	"quantvista/common"
	"quantvista/datasource"
	"quantvista/model"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// WatchlistService 自选股分组与条目。所有操作按 userID 隔离，跨用户访问一律视为不存在。
type WatchlistService struct {
	market *MarketService
}

func NewWatchlistService(market *MarketService) *WatchlistService {
	return &WatchlistService{market: market}
}

func watchlistRequestContext(contexts ...context.Context) context.Context {
	if len(contexts) > 0 && contexts[0] != nil {
		return contexts[0]
	}
	return context.Background()
}

// 结构变更统一先锁分组、再锁条目；删除分组必须与添加/移入/导入共享同一父行锁。
func lockedWatchlistGroup(tx *gorm.DB, userID, groupID int64) (*model.Watchlist, error) {
	var group model.Watchlist
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ? AND user_id = ?", groupID, userID).First(&group).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, errors.New("自选分组不存在")
		}
		return nil, err
	}
	return &group, nil
}

func lockedWritableWatchlistItem(tx *gorm.DB, userID, itemID, sourceGroupID, targetGroupID int64, item *model.WatchlistItem) error {
	ids := []int64{sourceGroupID}
	if targetGroupID != 0 && targetGroupID != sourceGroupID {
		ids = append(ids, targetGroupID)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	for _, id := range ids {
		if _, err := lockedWatchlistGroup(tx, userID, id); err != nil {
			return err
		}
	}
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ? AND user_id = ?", itemID, userID).First(item).Error; err != nil {
		return err
	}
	if item.WatchlistID != sourceGroupID {
		return errors.New("自选条目所在分组已变化，请刷新后重试")
	}
	return nil
}

var validPortfolioMarket = map[string]bool{"cn": true, "us": true, "hk": true}

// normalizeSymbolMarket 规整标的：symbol 去空格、market 小写并校验。
func normalizeSymbolMarket(symbol, market string) (string, string, error) {
	symbol = strings.TrimSpace(symbol)
	market = strings.ToLower(strings.TrimSpace(market))
	if market == "" {
		market = "cn"
	}
	if symbol == "" {
		return "", "", errors.New("股票代码不能为空")
	}
	if len(symbol) > 16 {
		return "", "", errors.New("股票代码过长")
	}
	if !validPortfolioMarket[market] {
		return "", "", errors.New("非法的市场")
	}
	return symbol, market, nil
}

// resolveName 尝试用行情校验代码并取名称；代码非法则拒绝，数据源临时不可用则放行（name 回退给定值）。
func (s *WatchlistService) resolveName(ctx context.Context, market, symbol, fallback string) (string, error) {
	q, err := s.market.GetQuote(ctx, market, symbol)
	if err == nil && q != nil && q.Name != "" {
		return q.Name, nil
	}
	if errors.Is(err, datasource.ErrSymbolInvalid) {
		return "", errors.New("无法识别的股票代码")
	}
	// 数据源临时不可用：不阻断添加，名称回退。
	return strings.TrimSpace(fallback), nil
}

// --- 分组 ---

// WatchlistItemView 条目 + 实时行情富化。展示行保留最近已知价（QuotesFor 展示链路），
// FreshnessStatus 标注时效——stale 行须由前端按「截至 DataTime」展示，不冒充实时；
// 推荐/分析等敏感消费方另行走 FreshQuotesFor（不依赖本视图的价）。
type WatchlistItemView struct {
	model.WatchlistItem
	Price           float64   `json:"price"`
	ChangePct       float64   `json:"change_pct"`
	QuoteOK         bool      `json:"quote_ok"`
	DataTime        time.Time `json:"data_time"`
	FreshnessStatus string    `json:"freshness_status,omitempty"` // fresh | stale | unknown
}

// WatchlistGroupView 分组 + 其条目。
type WatchlistGroupView struct {
	model.Watchlist
	Items []WatchlistItemView `json:"items"`
}

// EnsureDefaultGroup 用户无任何分组时建一个"默认分组"，保证前端总有落点。
func (s *WatchlistService) EnsureDefaultGroup(userID int64, contexts ...context.Context) (*model.Watchlist, error) {
	ctx := watchlistRequestContext(contexts...)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if common.DB == nil || userID <= 0 {
		return nil, errors.New("数据库不可用或用户无效")
	}
	var existing model.Watchlist
	db := common.DB.WithContext(ctx)
	err := db.Where("user_id = ?", userID).Order("sort_order, id").First(&existing).Error
	if err == nil {
		return &existing, nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, err
	}
	group := model.Watchlist{UserID: userID, Name: "默认分组", SortOrder: 0, InitialGroupKey: &userID}
	if err := db.Clauses(clause.OnConflict{DoNothing: true}).Create(&group).Error; err != nil {
		return nil, err
	}
	// 独立读取已提交结果，不能在 MySQL 插入等待之前固定“无分组”的可重复读快照。
	var result model.Watchlist
	err = db.Where("user_id = ?", userID).Order("sort_order, id").First(&result).Error
	return &result, err
}

// List 返回用户全部分组（含条目，条目富化实时行情）。
func (s *WatchlistService) List(ctx context.Context, userID int64) ([]WatchlistGroupView, error) {
	ctx = watchlistRequestContext(ctx)
	if _, err := s.EnsureDefaultGroup(userID, ctx); err != nil {
		return nil, err
	}
	var groups []model.Watchlist
	var items []model.WatchlistItem
	if err := readSnapshotTx(ctx, func(tx *gorm.DB) error {
		if err := tx.Where("user_id = ?", userID).Order("sort_order, id").Find(&groups).Error; err != nil {
			return err
		}
		return tx.Where("user_id = ?", userID).Order("is_pinned DESC, id").Find(&items).Error
	}); err != nil {
		return nil, err
	}

	// 一次并发取全部条目的行情。
	refs := make([]QuoteRef, 0, len(items))
	seen := map[string]bool{}
	for _, it := range items {
		k := QuoteKey(it.Market, it.Symbol)
		if !seen[k] {
			seen[k] = true
			refs = append(refs, QuoteRef{Market: it.Market, Symbol: it.Symbol})
		}
	}
	quotes := s.market.QuotesFor(ctx, refs)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	judge := s.market.FreshnessJudge("cn")

	byGroup := make(map[int64][]WatchlistItemView, len(groups))
	for _, it := range items {
		v := WatchlistItemView{WatchlistItem: it}
		if q := quotes[QuoteKey(it.Market, it.Symbol)]; q != nil {
			v.Price = q.Price
			v.ChangePct = q.ChangePct
			v.QuoteOK = true
			v.DataTime = q.DataTime
			if it.Market == "cn" {
				v.FreshnessStatus = judge(q.DataTime).Status
			} else {
				v.FreshnessStatus = freshStatusUnknown
			}
		}
		byGroup[it.WatchlistID] = append(byGroup[it.WatchlistID], v)
	}

	out := make([]WatchlistGroupView, 0, len(groups))
	for _, g := range groups {
		items := byGroup[g.ID]
		if items == nil {
			items = []WatchlistItemView{} // 空分组返回 [] 而非 null，前端无需判空
		}
		out = append(out, WatchlistGroupView{Watchlist: g, Items: items})
	}
	return out, nil
}

// CreateGroup 新建分组。
func (s *WatchlistService) CreateGroup(userID int64, name string, contexts ...context.Context) (*model.Watchlist, error) {
	ctx := watchlistRequestContext(contexts...)
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, errors.New("分组名称不能为空")
	}
	if len([]rune(name)) > 32 {
		return nil, errors.New("分组名称过长（最多 32 字）")
	}
	g := &model.Watchlist{UserID: userID, Name: name}
	if err := common.DB.WithContext(ctx).Create(g).Error; err != nil {
		return nil, err
	}
	return g, nil
}

// UpdateGroup 重命名/调整排序（仅本人）。
func (s *WatchlistService) UpdateGroup(userID, id int64, name string, sortOrder *int, contexts ...context.Context) (*model.Watchlist, error) {
	ctx := watchlistRequestContext(contexts...)
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, errors.New("分组名称不能为空")
	}
	if len([]rune(name)) > 32 {
		return nil, errors.New("分组名称过长（最多 32 字）")
	}
	updates := map[string]any{"name": name}
	if sortOrder != nil {
		updates["sort_order"] = *sortOrder
	}
	var group *model.Watchlist
	err := common.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var err error
		group, err = lockedWatchlistGroup(tx, userID, id)
		if err != nil {
			return err
		}
		if err := tx.Model(group).Updates(updates).Error; err != nil {
			return err
		}
		return nil
	})
	return group, err
}

// DeleteGroup 删除分组及其条目（仅本人，事务保证一致）。
func (s *WatchlistService) DeleteGroup(userID, id int64, contexts ...context.Context) error {
	return common.DB.WithContext(watchlistRequestContext(contexts...)).Transaction(func(tx *gorm.DB) error {
		g, err := lockedWatchlistGroup(tx, userID, id)
		if err != nil {
			return err
		}
		if err := tx.Where("user_id = ? AND watchlist_id = ?", userID, id).Delete(&model.WatchlistItem{}).Error; err != nil {
			return err
		}
		return tx.Where("id = ? AND user_id = ?", g.ID, userID).Delete(&model.Watchlist{}).Error
	})
}

// --- 条目 ---

// WatchlistItemInput 新增/编辑条目入参。
type WatchlistItemInput struct {
	Symbol      string `json:"symbol"`
	Market      string `json:"market"`
	Name        string `json:"name"`
	Note        string `json:"note"`
	FocusReason string `json:"focus_reason"`
	IsPinned    bool   `json:"is_pinned"`
	WatchlistID int64  `json:"watchlist_id"` // 编辑时可用于移动分组
}

// 更新仅修改显式提交的字段；重点按钮不能覆盖另一次编辑留下的备注。
type WatchlistItemUpdateInput struct {
	Note        *string `json:"note"`
	FocusReason *string `json:"focus_reason"`
	IsPinned    *bool   `json:"is_pinned"`
	WatchlistID *int64  `json:"watchlist_id"`
}

// AddItem 向分组添加条目。分组须属本人；同组同标的重复报错。
func (s *WatchlistService) AddItem(ctx context.Context, userID, groupID int64, in WatchlistItemInput) (*model.WatchlistItem, error) {
	ctx = watchlistRequestContext(ctx)
	var g model.Watchlist
	if err := common.DB.WithContext(ctx).Where("id = ? AND user_id = ?", groupID, userID).First(&g).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, errors.New("分组不存在")
		}
		return nil, err
	}
	symbol, market, err := normalizeSymbolMarket(in.Symbol, in.Market)
	if err != nil {
		return nil, err
	}
	name, err := s.resolveName(ctx, market, symbol, in.Name)
	if err != nil {
		return nil, err
	}

	item := &model.WatchlistItem{
		UserID:      userID,
		WatchlistID: groupID,
		Symbol:      symbol,
		Market:      market,
		Name:        truncateRunes(name, 64),
		Note:        truncateRunes(strings.TrimSpace(in.Note), 500),
		FocusReason: truncateRunes(strings.TrimSpace(in.FocusReason), 500),
		IsPinned:    in.IsPinned,
	}
	if err := common.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if _, err := lockedWatchlistGroup(tx, userID, groupID); err != nil {
			return err
		}
		var exists int64
		if err := tx.Model(&model.WatchlistItem{}).
			Where("user_id = ? AND watchlist_id = ? AND symbol = ? AND market = ?", userID, groupID, symbol, market).
			Count(&exists).Error; err != nil {
			return err
		}
		if exists > 0 {
			return errors.New("该股票已在此分组中")
		}
		if err := tx.Create(item).Error; err != nil {
			return err
		}
		return setOnboardingStepTx(tx, userID, OnboardingStepPortfolio, model.OnboardingStepCompleted, 0)
	}); err != nil {
		return nil, err
	}
	return item, nil
}

// UpdateItem 编辑条目：备注/关注原因/重点关注，或移动到另一分组（均须属本人）。
func (s *WatchlistService) UpdateItem(userID, itemID int64, in WatchlistItemUpdateInput, contexts ...context.Context) (*model.WatchlistItem, error) {
	db := common.DB.WithContext(watchlistRequestContext(contexts...))
	// 定位放在写事务外，避免在等待分组锁前固定 MySQL 读视图；锁内复验源组归属。
	var scope struct{ WatchlistID int64 }
	if err := db.Model(&model.WatchlistItem{}).Select("watchlist_id").Where("id = ? AND user_id = ?", itemID, userID).Take(&scope).Error; err != nil {
		return nil, err
	}
	var item model.WatchlistItem
	targetGroupID := int64(0)
	if in.WatchlistID != nil {
		targetGroupID = *in.WatchlistID
	}
	if err := db.Transaction(func(tx *gorm.DB) error {
		if err := lockedWritableWatchlistItem(tx, userID, itemID, scope.WatchlistID, targetGroupID, &item); err != nil {
			return err
		}
		// 移动分组：校验目标分组属本人，且目标分组无同标的。
		if targetGroupID != 0 && targetGroupID != item.WatchlistID {
			var dup int64
			if err := tx.Model(&model.WatchlistItem{}).
				Where("user_id = ? AND watchlist_id = ? AND symbol = ? AND market = ?", userID, targetGroupID, item.Symbol, item.Market).
				Count(&dup).Error; err != nil {
				return err
			}
			if dup > 0 {
				return errors.New("目标分组已有该股票")
			}
			item.WatchlistID = targetGroupID
		}
		if in.Note != nil {
			item.Note = truncateRunes(strings.TrimSpace(*in.Note), 500)
		}
		if in.FocusReason != nil {
			item.FocusReason = truncateRunes(strings.TrimSpace(*in.FocusReason), 500)
		}
		if in.IsPinned != nil {
			item.IsPinned = *in.IsPinned
		}
		return tx.Save(&item).Error
	}); err != nil {
		return nil, err
	}
	return &item, nil
}

// DeleteItem 删除自选条目（仅本人）。
func (s *WatchlistService) DeleteItem(userID, itemID int64, contexts ...context.Context) error {
	return common.DB.WithContext(watchlistRequestContext(contexts...)).Transaction(func(tx *gorm.DB) error {
		var item model.WatchlistItem
		query := tx.Where("id = ? AND user_id = ?", itemID, userID)
		if !common.UsingSQLite {
			query = query.Clauses(clause.Locking{Strength: "UPDATE"})
		}
		if err := query.First(&item).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return errors.New("自选条目不存在")
			}
			return err
		}
		res := tx.Where("id = ? AND user_id = ?", itemID, userID).Delete(&model.WatchlistItem{})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected != 1 {
			return errors.New("自选条目已发生变化，请重试")
		}
		return nil
	})
}

// --- 机会池漏斗 ---

var validResearchStage = map[string]bool{
	"":                      true, // 清除标注
	model.StageDiscovered:   true,
	model.StageScreening:    true,
	model.StageWatching:     true,
	model.StageWaitingPrice: true,
	model.StagePlanned:      true,
	model.StageBought:       true,
	model.StagePassed:       true,
	model.StageReviewed:     true,
}

// SetItemStage 流转研究阶段。转 passed 时记录当时现价与原因（错过机会复盘的基准）；
// 从 passed 转出时保留历史价格（复盘价值在于「当时放弃时的价」，覆盖即失真）。
func (s *WatchlistService) SetItemStage(ctx context.Context, userID, itemID int64, stage, reason string) (*model.WatchlistItem, error) {
	ctx = watchlistRequestContext(ctx)
	if !validResearchStage[stage] {
		return nil, errors.New("无效的研究阶段")
	}
	var initial model.WatchlistItem
	if err := common.DB.WithContext(ctx).Where("id = ? AND user_id = ?", itemID, userID).First(&initial).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, errors.New("自选条目不存在")
		}
		return nil, err
	}
	passedPrice := float64(0)
	if stage == model.StagePassed && initial.ResearchStage != model.StagePassed {
		// 放弃价 fail-closed：只记当前有效（fresh）行情——旧价会成为永久落库的错误
		// 复盘基准（后续「错过机会」结论整体失真）；取不到 fresh 记 0（显示"无基准价"）。
		if q, fi, err := s.market.GetFreshQuote(ctx, initial.Market, initial.Symbol); err == nil &&
			q != nil && q.Price > 0 && fi.Status == freshStatusFresh {
			passedPrice = q.Price
		}
	}
	var item model.WatchlistItem
	if err := common.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		query := tx.Where("id = ? AND user_id = ?", itemID, userID)
		if !common.UsingSQLite {
			query = query.Clauses(clause.Locking{Strength: "UPDATE"})
		}
		if err := query.First(&item).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return errors.New("自选条目不存在")
			}
			return err
		}
		changed := item.ResearchStage != stage
		if changed && stage == model.StagePassed && initial.ResearchStage == model.StagePassed {
			return errors.New("研究阶段已发生变化，请刷新后重新标记放弃")
		}
		item.ResearchStage = stage
		if changed {
			now := time.Now()
			item.StageAt = &now
		}
		if stage == model.StagePassed {
			item.PassedReason = truncateRunes(strings.TrimSpace(reason), 250)
			if changed {
				item.PassedPrice = passedPrice
			}
		}
		return tx.Save(&item).Error
	}); err != nil {
		return nil, err
	}
	return &item, nil
}

// MissedOpportunityView 错过机会复盘行：放弃时价格 vs 现价。
type MissedOpportunityView struct {
	model.WatchlistItem
	CurrentPrice   float64 `json:"current_price"`
	QuoteOK        bool    `json:"quote_ok"`
	ChangeSincePct float64 `json:"change_since_pct"` // 放弃后涨跌幅（现价 vs 放弃价）
	Verdict        string  `json:"verdict"`          // avoided_loss / missed_gain / neutral / no_base / stale_quote

	QuoteAsOf      string  `json:"quote_as_of,omitempty"` // 行情数据源时刻（stale 时为最近已知）
	LastPrice      float64 `json:"last_price,omitempty"`  // 最近已知价（stale 展示用，不参与结论）
	ComparisonNote string  `json:"comparison_note,omitempty"`
}

// 错过机会判定阈值：放弃后涨/跌超过该幅度（%）才计为「错过上涨/回避正确」。
const missedVerdictPct = 5.0

// missedVerdict 纯函数：按放弃价与现价判定复盘结论。
func missedVerdict(passedPrice, currentPrice float64) (pct float64, verdict string) {
	if passedPrice <= 0 || currentPrice <= 0 {
		return 0, "no_base"
	}
	pct = round2((currentPrice - passedPrice) / passedPrice * 100)
	switch {
	case pct >= missedVerdictPct:
		return pct, "missed_gain" // 放弃后上涨：错过机会
	case pct <= -missedVerdictPct:
		return pct, "avoided_loss" // 放弃后下跌：回避正确
	default:
		return pct, "neutral"
	}
}

// MissedOpportunities 已放弃标的的复盘视图：验证「是正确回避风险，还是错过机会」。
// fail-closed：结论只建立在当前有效行情上——stale 行情的「错过上涨/回避正确」是
// 建立在旧价上的假结论，标 stale_quote 并保留最近已知价供参考。
func (s *WatchlistService) MissedOpportunities(ctx context.Context, userID int64) ([]MissedOpportunityView, error) {
	ctx = watchlistRequestContext(ctx)
	items, notes, err := readMissedComparisons(ctx, userID)
	if err != nil {
		return nil, err
	}
	refs := make([]QuoteRef, 0, len(items))
	for _, it := range items {
		refs = append(refs, QuoteRef{Market: it.Market, Symbol: it.Symbol})
	}
	quotes := s.market.FreshQuotesFor(ctx, refs)
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	out := make([]MissedOpportunityView, 0, len(items))
	for _, it := range items {
		v := MissedOpportunityView{WatchlistItem: it, Verdict: "no_base"}
		if it.PassedPrice > 0 {
			v.Verdict = "no_quote"
		}
		if fq, ok := quotes[QuoteKey(it.Market, it.Symbol)]; ok && fq.Quote != nil && fq.Quote.Price > 0 {
			if !fq.Quote.DataTime.IsZero() {
				v.QuoteAsOf = fq.Quote.DataTime.In(time.Local).Format("2006-01-02 15:04")
			}
			if fq.Fresh.Status == freshStatusFresh {
				v.CurrentPrice = fq.Quote.Price
				v.QuoteOK = true
				if note := notes[it.ID]; note != "" && it.PassedPrice > 0 {
					v.Verdict, v.ComparisonNote = "comparison_unknown", note
				} else {
					v.ChangeSincePct, v.Verdict = missedVerdict(it.PassedPrice, fq.Quote.Price)
				}
			} else {
				v.Verdict = "stale_quote"
				v.LastPrice = fq.Quote.Price
			}
		}
		out = append(out, v)
	}
	return out, nil
}
