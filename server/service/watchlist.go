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
		Note:        strings.TrimSpace(in.Note),
		FocusReason: strings.TrimSpace(in.FocusReason),
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
	item.Note = strings.TrimSpace(in.Note)
	item.FocusReason = strings.TrimSpace(in.FocusReason)
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
