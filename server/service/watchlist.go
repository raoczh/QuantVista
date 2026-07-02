package service

import (
	"context"
	"errors"
	"strings"
	"time"

	"quantvista/common"
	"quantvista/datasource"
	"quantvista/model"

	"gorm.io/gorm"
)

// WatchlistService 自选股分组与条目。所有操作按 userID 隔离，跨用户访问一律视为不存在。
type WatchlistService struct {
	market *MarketService
}

func NewWatchlistService(market *MarketService) *WatchlistService {
	return &WatchlistService{market: market}
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
	if err == nil && q.Name != "" {
		return q.Name, nil
	}
	if errors.Is(err, datasource.ErrSymbolInvalid) {
		return "", errors.New("无法识别的股票代码")
	}
	// 数据源临时不可用：不阻断添加，名称回退。
	return strings.TrimSpace(fallback), nil
}

// --- 分组 ---

// WatchlistItemView 条目 + 实时行情富化。
type WatchlistItemView struct {
	model.WatchlistItem
	Price     float64   `json:"price"`
	ChangePct float64   `json:"change_pct"`
	QuoteOK   bool      `json:"quote_ok"`
	DataTime  time.Time `json:"data_time"`
}

// WatchlistGroupView 分组 + 其条目。
type WatchlistGroupView struct {
	model.Watchlist
	Items []WatchlistItemView `json:"items"`
}

// EnsureDefaultGroup 用户无任何分组时建一个"默认分组"，保证前端总有落点。
func (s *WatchlistService) EnsureDefaultGroup(userID int64) (*model.Watchlist, error) {
	var count int64
	if err := common.DB.Model(&model.Watchlist{}).Where("user_id = ?", userID).Count(&count).Error; err != nil {
		return nil, err
	}
	if count > 0 {
		var g model.Watchlist
		err := common.DB.Where("user_id = ?", userID).Order("sort_order, id").First(&g).Error
		return &g, err
	}
	g := &model.Watchlist{UserID: userID, Name: "默认分组", SortOrder: 0}
	if err := common.DB.Create(g).Error; err != nil {
		return nil, err
	}
	return g, nil
}

// List 返回用户全部分组（含条目，条目富化实时行情）。
func (s *WatchlistService) List(ctx context.Context, userID int64) ([]WatchlistGroupView, error) {
	if _, err := s.EnsureDefaultGroup(userID); err != nil {
		return nil, err
	}
	var groups []model.Watchlist
	if err := common.DB.Where("user_id = ?", userID).Order("sort_order, id").Find(&groups).Error; err != nil {
		return nil, err
	}
	var items []model.WatchlistItem
	if err := common.DB.Where("user_id = ?", userID).Order("is_pinned DESC, id").Find(&items).Error; err != nil {
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

	byGroup := make(map[int64][]WatchlistItemView, len(groups))
	for _, it := range items {
		v := WatchlistItemView{WatchlistItem: it}
		if q := quotes[QuoteKey(it.Market, it.Symbol)]; q != nil {
			v.Price = q.Price
			v.ChangePct = q.ChangePct
			v.QuoteOK = true
			v.DataTime = q.DataTime
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
func (s *WatchlistService) CreateGroup(userID int64, name string) (*model.Watchlist, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, errors.New("分组名称不能为空")
	}
	if len([]rune(name)) > 32 {
		return nil, errors.New("分组名称过长（最多 32 字）")
	}
	g := &model.Watchlist{UserID: userID, Name: name}
	if err := common.DB.Create(g).Error; err != nil {
		return nil, err
	}
	return g, nil
}

// UpdateGroup 重命名/调整排序（仅本人）。
func (s *WatchlistService) UpdateGroup(userID, id int64, name string, sortOrder int) (*model.Watchlist, error) {
	var g model.Watchlist
	if err := common.DB.Where("id = ? AND user_id = ?", id, userID).First(&g).Error; err != nil {
		return nil, errors.New("分组不存在")
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, errors.New("分组名称不能为空")
	}
	if len([]rune(name)) > 32 {
		return nil, errors.New("分组名称过长（最多 32 字）")
	}
	g.Name = name
	g.SortOrder = sortOrder
	if err := common.DB.Save(&g).Error; err != nil {
		return nil, err
	}
	return &g, nil
}

// DeleteGroup 删除分组及其条目（仅本人，事务保证一致）。
func (s *WatchlistService) DeleteGroup(userID, id int64) error {
	var g model.Watchlist
	if err := common.DB.Where("id = ? AND user_id = ?", id, userID).First(&g).Error; err != nil {
		return errors.New("分组不存在")
	}
	return common.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("user_id = ? AND watchlist_id = ?", userID, id).Delete(&model.WatchlistItem{}).Error; err != nil {
			return err
		}
		return tx.Delete(&g).Error
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

// AddItem 向分组添加条目。分组须属本人；同组同标的重复报错。
func (s *WatchlistService) AddItem(ctx context.Context, userID, groupID int64, in WatchlistItemInput) (*model.WatchlistItem, error) {
	var g model.Watchlist
	if err := common.DB.Where("id = ? AND user_id = ?", groupID, userID).First(&g).Error; err != nil {
		return nil, errors.New("分组不存在")
	}
	symbol, market, err := normalizeSymbolMarket(in.Symbol, in.Market)
	if err != nil {
		return nil, err
	}
	name, err := s.resolveName(ctx, market, symbol, in.Name)
	if err != nil {
		return nil, err
	}

	var exists int64
	common.DB.Model(&model.WatchlistItem{}).
		Where("user_id = ? AND watchlist_id = ? AND symbol = ? AND market = ?", userID, groupID, symbol, market).
		Count(&exists)
	if exists > 0 {
		return nil, errors.New("该股票已在此分组中")
	}

	item := &model.WatchlistItem{
		UserID:      userID,
		WatchlistID: groupID,
		Symbol:      symbol,
		Market:      market,
		Name:        name,
		Note:        truncateRunes(strings.TrimSpace(in.Note), 500),
		FocusReason: truncateRunes(strings.TrimSpace(in.FocusReason), 500),
		IsPinned:    in.IsPinned,
	}
	if err := common.DB.Create(item).Error; err != nil {
		return nil, errors.New("添加失败：" + err.Error())
	}
	return item, nil
}

// UpdateItem 编辑条目：备注/关注原因/重点关注，或移动到另一分组（均须属本人）。
func (s *WatchlistService) UpdateItem(userID, itemID int64, in WatchlistItemInput) (*model.WatchlistItem, error) {
	var item model.WatchlistItem
	if err := common.DB.Where("id = ? AND user_id = ?", itemID, userID).First(&item).Error; err != nil {
		return nil, errors.New("自选条目不存在")
	}
	// 移动分组：校验目标分组属本人，且目标分组无同标的。
	if in.WatchlistID != 0 && in.WatchlistID != item.WatchlistID {
		var g model.Watchlist
		if err := common.DB.Where("id = ? AND user_id = ?", in.WatchlistID, userID).First(&g).Error; err != nil {
			return nil, errors.New("目标分组不存在")
		}
		var dup int64
		common.DB.Model(&model.WatchlistItem{}).
			Where("user_id = ? AND watchlist_id = ? AND symbol = ? AND market = ?", userID, in.WatchlistID, item.Symbol, item.Market).
			Count(&dup)
		if dup > 0 {
			return nil, errors.New("目标分组已有该股票")
		}
		item.WatchlistID = in.WatchlistID
	}
	item.Note = truncateRunes(strings.TrimSpace(in.Note), 500)
	item.FocusReason = truncateRunes(strings.TrimSpace(in.FocusReason), 500)
	item.IsPinned = in.IsPinned
	if err := common.DB.Save(&item).Error; err != nil {
		return nil, err
	}
	return &item, nil
}

// DeleteItem 删除自选条目（仅本人）。
func (s *WatchlistService) DeleteItem(userID, itemID int64) error {
	res := common.DB.Where("id = ? AND user_id = ?", itemID, userID).Delete(&model.WatchlistItem{})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return errors.New("自选条目不存在")
	}
	return nil
}

// --- 机会池漏斗 ---

var validResearchStage = map[string]bool{
	"": true, // 清除标注
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
	if !validResearchStage[stage] {
		return nil, errors.New("无效的研究阶段")
	}
	var item model.WatchlistItem
	if err := common.DB.Where("id = ? AND user_id = ?", itemID, userID).First(&item).Error; err != nil {
		return nil, errors.New("自选条目不存在")
	}
	item.ResearchStage = stage
	now := time.Now()
	item.StageAt = &now
	if stage == model.StagePassed {
		item.PassedReason = truncateRunes(strings.TrimSpace(reason), 250)
		// 放弃价 best-effort：行情不可用时记 0（复盘时显示"无基准价"）。
		if q, err := s.market.GetQuote(ctx, item.Market, item.Symbol); err == nil {
			item.PassedPrice = q.Price
		}
	}
	if err := common.DB.Save(&item).Error; err != nil {
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
	Verdict        string  `json:"verdict"`          // avoided_loss / missed_gain / neutral / no_base
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
func (s *WatchlistService) MissedOpportunities(ctx context.Context, userID int64) ([]MissedOpportunityView, error) {
	var items []model.WatchlistItem
	if err := common.DB.Where("user_id = ? AND research_stage = ?", userID, model.StagePassed).
		Order("stage_at DESC").Find(&items).Error; err != nil {
		return nil, err
	}
	refs := make([]QuoteRef, 0, len(items))
	for _, it := range items {
		refs = append(refs, QuoteRef{Market: it.Market, Symbol: it.Symbol})
	}
	quotes := s.market.QuotesFor(ctx, refs)

	out := make([]MissedOpportunityView, 0, len(items))
	for _, it := range items {
		v := MissedOpportunityView{WatchlistItem: it, Verdict: "no_base"}
		if q := quotes[QuoteKey(it.Market, it.Symbol)]; q != nil {
			v.CurrentPrice = q.Price
			v.QuoteOK = true
			v.ChangeSincePct, v.Verdict = missedVerdict(it.PassedPrice, q.Price)
		}
		out = append(out, v)
	}
	return out, nil
}
