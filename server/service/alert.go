package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"quantvista/common"
	"quantvista/datasource"
	"quantvista/model"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// AlertService 条件提醒：管理规则 + 按行情/日线评估命中（命中判定用当日 OHLC 避免漏判盘中触达）。
// 命中默认仅落库供待办/页面高亮；若用户配置了启用的推送通道，则额外主动推送（同日去重）。
type AlertService struct {
	market alertMarketProvider
	notify alertNotifier
}

type alertMarketProvider interface {
	GetQuote(context.Context, string, string) (*datasource.Quote, error)
	GetFreshQuote(context.Context, string, string) (*datasource.Quote, quoteFreshInfo, error)
	GetDailyBars(context.Context, string, string, int) ([]datasource.Bar, error)
	GetValuation(context.Context, string, string) (*datasource.Valuation, error)
	QuoteFreshnessOf(string, time.Time) quoteFreshInfo
	// FreshQuotesFor 批量新鲜行情（D14/D15 持仓类规则用：一个用户一次拉完全部持仓，
	// 绝不逐仓调 GetFreshQuote——持仓数可达几十，逐只调会把上游打满）。
	FreshQuotesFor(context.Context, []QuoteRef) map[string]FreshQuoteResult
}

type alertNotifier interface {
	SendMsgContext(context.Context, int64, NotifyMessage)
}

func NewAlertService(market *MarketService) *AlertService {
	return &AlertService{market: market, notify: NewNotifyService()}
}

const (
	alertBarLimit     = 90 // 评估 ma/breakout/volume_surge 需要的日线条数
	maxAlertsPerUser  = 200
	volumeAvgWindow   = 20  // 放量判定的均量窗口（交易日）
	alertEventMaxList = 500 // 命中历史单次最多返回条数
	// 推送刷新脱离已过期的评估 context，但整批仍受这个总预算约束。
	alertNotifyFlushTimeout = 15 * time.Second
)

var validAlertKind = map[string]bool{
	model.AlertKindPrice:        true,
	model.AlertKindPctChange:    true,
	model.AlertKindMA:           true,
	model.AlertKindBreakout:     true,
	model.AlertKindVolumeSurge:  true,
	model.AlertKindAmplitude:    true,
	model.AlertKindEarnDate:     true,
	model.AlertKindEarnFcst:     true,
	model.AlertKindCostGain:     true,
	model.AlertKindCostDrawdown: true,
	model.AlertKindPeakDrawdown: true,
}

// earnAlertKinds 财报日历类 kind：盘中高频行情评估显式排除（否则会被拉去每
// symbol 拉行情空转），由财报刷新 job 每日一评 + 手动「立即检查」时顺带评估（查本地表零上游成本）。
var earnAlertKinds = []string{model.AlertKindEarnDate, model.AlertKindEarnFcst}

// positionAlertKinds 持仓卖出决策类 kind（D14/D15）：**评估单元是「一笔持仓」而非
// 「一个 symbol」**——规则可不绑 symbol（= 我的全部持仓），成本取 positions.buy_price。
// 盘中轮照评（它们要盯的就是盘中价），但走 evaluatePositionRules 这条独立编排，
// 不能混进按 rule.Symbol 拉行情的 evaluateRules（symbol 为空会拉挂）。
var positionAlertKinds = []string{model.AlertKindCostGain, model.AlertKindCostDrawdown, model.AlertKindPeakDrawdown}

// nonPositionMarketKinds 走 evaluateRules 的行情类 kind（= 全部 kind − 财报类 − 持仓类）。
var nonPositionMarketKinds = append(append([]string{}, earnAlertKinds...), positionAlertKinds...)

func isEarnAlertKind(kind string) bool {
	return kind == model.AlertKindEarnDate || kind == model.AlertKindEarnFcst
}

// isPositionAlertKind 是否为持仓卖出决策类（成本/峰值口径，可绑全部持仓）。
func isPositionAlertKind(kind string) bool {
	return kind == model.AlertKindCostGain || kind == model.AlertKindCostDrawdown ||
		kind == model.AlertKindPeakDrawdown
}

// alertKindNeedsBars 该类型评估是否需要日线序列。
func alertKindNeedsBars(kind string) bool {
	return kind == model.AlertKindMA || kind == model.AlertKindBreakout || kind == model.AlertKindVolumeSurge
}

// alertEval 评估的输入观测值（纯函数便于测试）。
type alertEval struct {
	Price     float64
	DayHigh   float64
	DayLow    float64
	ChangePct float64
	DayVolume int64     // 当日成交量（手），volume_surge 用
	Amplitude float64   // 当日振幅 %，amplitude 用（估值源缺失时由 quote 计算）
	Closes    []float64 // 升序日线收盘（最新在末尾），供 ma/breakout
	Highs     []float64
	Lows      []float64
	Volumes   []int64 // 升序日线成交量（手，剔除当日在途 bar），供 volume_surge 均量
}

// evaluateAlert 纯函数判定规则是否命中，返回（命中、观测值、命中说明）。
// 数据不足（如 ma/breakout 缺日线）时不命中。
func evaluateAlert(rule model.AlertRule, in alertEval) (bool, float64, string) {
	switch rule.Kind {
	case model.AlertKindPrice:
		if rule.Op == model.AlertOpGTE {
			hi := in.DayHigh
			if hi <= 0 {
				hi = in.Price
			}
			if hi >= rule.Threshold {
				return true, in.Price, fmt.Sprintf("当日最高 %.2f 触及目标价 ≥ %.2f", hi, rule.Threshold)
			}
		} else {
			lo := in.DayLow
			if lo <= 0 {
				lo = in.Price
			}
			if lo <= rule.Threshold {
				return true, in.Price, fmt.Sprintf("当日最低 %.2f 触及目标价 ≤ %.2f", lo, rule.Threshold)
			}
		}
		return false, in.Price, ""

	case model.AlertKindPctChange:
		if rule.Op == model.AlertOpGTE {
			if in.ChangePct >= rule.Threshold {
				return true, in.ChangePct, fmt.Sprintf("当日涨幅 %.2f%% ≥ %.2f%%", in.ChangePct, rule.Threshold)
			}
		} else {
			if in.ChangePct <= rule.Threshold {
				return true, in.ChangePct, fmt.Sprintf("当日跌幅 %.2f%% ≤ %.2f%%", in.ChangePct, rule.Threshold)
			}
		}
		return false, in.ChangePct, ""

	case model.AlertKindMA:
		ma, ok := movingAverage(in.Closes, rule.Period)
		if !ok {
			return false, in.Price, ""
		}
		if rule.Op == model.AlertOpGTE {
			if in.Price >= ma {
				return true, in.Price, fmt.Sprintf("现价 %.2f 站上 MA%d（%.2f）", in.Price, rule.Period, ma)
			}
		} else {
			if in.Price <= ma {
				return true, in.Price, fmt.Sprintf("现价 %.2f 跌破 MA%d（%.2f）", in.Price, rule.Period, ma)
			}
		}
		return false, in.Price, ""

	case model.AlertKindBreakout:
		if rule.Op == model.AlertOpGTE {
			hh, ok := windowMax(in.Highs, rule.Period)
			if !ok {
				return false, in.Price, ""
			}
			hi := in.DayHigh
			if hi <= 0 {
				hi = in.Price
			}
			if hi >= hh {
				return true, in.Price, fmt.Sprintf("当日最高 %.2f 创近 %d 日新高（前高 %.2f）", hi, rule.Period, hh)
			}
		} else {
			ll, ok := windowMin(in.Lows, rule.Period)
			if !ok {
				return false, in.Price, ""
			}
			lo := in.DayLow
			if lo <= 0 {
				lo = in.Price
			}
			if lo <= ll {
				return true, in.Price, fmt.Sprintf("当日最低 %.2f 创近 %d 日新低（前低 %.2f）", lo, rule.Period, ll)
			}
		}
		return false, in.Price, ""

	case model.AlertKindVolumeSurge:
		// 当日量 vs 近 20 日均量的倍数（threshold=倍数）。均量窗口剔除当日在途 bar。
		avg, ok := volumeAverage(in.Volumes, volumeAvgWindow)
		if !ok || in.DayVolume <= 0 {
			return false, 0, ""
		}
		ratio := round2(float64(in.DayVolume) / avg)
		if rule.Op == model.AlertOpGTE {
			if ratio >= rule.Threshold {
				return true, ratio, fmt.Sprintf("当日量达 %d 日均量的 %.2f 倍 ≥ %.2f 倍（放量）", volumeAvgWindow, ratio, rule.Threshold)
			}
		} else {
			if ratio <= rule.Threshold {
				return true, ratio, fmt.Sprintf("当日量仅为 %d 日均量的 %.2f 倍 ≤ %.2f 倍（缩量）", volumeAvgWindow, ratio, rule.Threshold)
			}
		}
		return false, ratio, ""

	case model.AlertKindAmplitude:
		// 当日振幅 %（(high-low)/prev_close，优先取估值源自带值）。缺数据（0）不判定。
		if in.Amplitude <= 0 {
			return false, 0, ""
		}
		if rule.Op == model.AlertOpGTE {
			if in.Amplitude >= rule.Threshold {
				return true, in.Amplitude, fmt.Sprintf("当日振幅 %.2f%% ≥ %.2f%%", in.Amplitude, rule.Threshold)
			}
		} else {
			if in.Amplitude <= rule.Threshold {
				return true, in.Amplitude, fmt.Sprintf("当日振幅 %.2f%% ≤ %.2f%%", in.Amplitude, rule.Threshold)
			}
		}
		return false, in.Amplitude, ""
	}
	return false, in.Price, ""
}

// movingAverage 取末尾 period 个收盘的均值（不足则 false）。
func movingAverage(closes []float64, period int) (float64, bool) {
	if period <= 0 || len(closes) < period {
		return 0, false
	}
	sum := 0.0
	for _, c := range closes[len(closes)-period:] {
		sum += c
	}
	return round2(sum / float64(period)), true
}

// windowMax 取末尾 period 根的最高价上界（用于突破前高比较，排除当日：调用方用当日 high 与之比较）。
func windowMax(highs []float64, period int) (float64, bool) {
	if period <= 0 || len(highs) < period {
		return 0, false
	}
	m := 0.0
	for _, h := range highs[len(highs)-period:] {
		if h > m {
			m = h
		}
	}
	if m == 0 {
		return 0, false
	}
	return round2(m), true
}

func windowMin(lows []float64, period int) (float64, bool) {
	if period <= 0 || len(lows) < period {
		return 0, false
	}
	m := lows[len(lows)-period]
	for _, l := range lows[len(lows)-period:] {
		if l > 0 && l < m {
			m = l
		}
	}
	if m <= 0 {
		return 0, false
	}
	return round2(m), true
}

// volumeAverage 取末尾 period 个成交量的均值（不足或均量为 0 则 false）。
func volumeAverage(volumes []int64, period int) (float64, bool) {
	if period <= 0 || len(volumes) < period {
		return 0, false
	}
	sum := int64(0)
	for _, v := range volumes[len(volumes)-period:] {
		sum += v
	}
	if sum <= 0 {
		return 0, false
	}
	return float64(sum) / float64(period), true
}

// --- 校验 / CRUD ---

// AlertInput 新建/编辑规则入参。
type AlertInput struct {
	Symbol    string  `json:"symbol"`
	Market    string  `json:"market"`
	Name      string  `json:"name"`
	Kind      string  `json:"kind"`
	Op        string  `json:"op"`
	Threshold float64 `json:"threshold"`
	Period    int     `json:"period"`
	Once      bool    `json:"once"`
	Note      string  `json:"note"`
}

func (s *AlertService) validate(in *AlertInput) error {
	in.Kind = strings.ToLower(strings.TrimSpace(in.Kind))
	in.Op = strings.ToLower(strings.TrimSpace(in.Op))
	if !validAlertKind[in.Kind] {
		return errors.New("不支持的提醒类型")
	}
	if isEarnAlertKind(in.Kind) {
		in.Op = model.AlertOpGTE // 财报类无比较方向语义，统一存 gte（前端可不传）
	}
	if isPositionAlertKind(in.Kind) {
		// 持仓类各 kind 自带方向（涨够/跌够/回撤够），op 无语义统一存 gte；
		// **Once 强制 false**——一条规则覆盖多笔持仓，命中一笔就暂停整条规则
		// 会让其余持仓从此失联（这类规则的价值恰恰在于长期常驻）。
		in.Op = model.AlertOpGTE
		in.Once = false
	}
	if in.Op != model.AlertOpGTE && in.Op != model.AlertOpLTE {
		return errors.New("比较方向须为 gte 或 lte")
	}
	switch in.Kind {
	case model.AlertKindPrice:
		if in.Threshold <= 0 {
			return errors.New("目标价必须大于 0")
		}
	case model.AlertKindMA, model.AlertKindBreakout:
		if in.Period < 2 || in.Period > 250 {
			return errors.New("周期须在 2~250 之间")
		}
	case model.AlertKindVolumeSurge:
		if in.Threshold <= 0 || in.Threshold > 100 {
			return errors.New("量比倍数须在 0~100 之间")
		}
	case model.AlertKindAmplitude:
		if in.Threshold <= 0 || in.Threshold > 100 {
			return errors.New("振幅阈值须在 0~100 之间（%）")
		}
	case model.AlertKindEarnDate:
		if in.Threshold < 1 || in.Threshold > 30 {
			return errors.New("披露提前天数须在 1~30 之间")
		}
		in.Threshold = float64(int(in.Threshold))
	case model.AlertKindEarnFcst:
		in.Threshold = 0
	case model.AlertKindCostGain:
		// 相对成本的涨幅可以很大（翻倍股），上限放到 1000%。
		if in.Threshold <= 0 || in.Threshold > 1000 {
			return errors.New("相对成本涨幅须在 0~1000 之间（%）")
		}
	case model.AlertKindCostDrawdown, model.AlertKindPeakDrawdown:
		// 跌幅/回撤是正数百分比，最大不超过 100%（跌没了）。
		if in.Threshold <= 0 || in.Threshold >= 100 {
			return errors.New("跌幅/回撤须在 0~100 之间（%）")
		}
	}
	return nil
}

// positionAlertAllName 未绑定 symbol 的持仓类规则的展示名。
const positionAlertAllName = "我的全部持仓"

// Create 新建提醒规则（校验代码 + 取名）。
// **持仓卖出决策类允许 symbol 为空**（= 我的全部持仓），此时不校验代码、不拉行情取名。
func (s *AlertService) Create(ctx context.Context, userID int64, in AlertInput) (*model.AlertRule, error) {
	if err := s.validate(&in); err != nil {
		return nil, err
	}
	allPositions := isPositionAlertKind(in.Kind) && strings.TrimSpace(in.Symbol) == ""
	symbol, market := "", strings.TrimSpace(in.Market)
	if allPositions {
		if market == "" {
			market = "cn" // 持仓类只覆盖 A 股（与 guard/事件日历同口径）
		}
	} else {
		var err error
		symbol, market, err = normalizeSymbolMarket(in.Symbol, in.Market)
		if err != nil {
			return nil, err
		}
	}
	var cnt int64
	common.DB.Model(&model.AlertRule{}).Where("user_id = ?", userID).Count(&cnt)
	if cnt >= maxAlertsPerUser {
		return nil, fmt.Errorf("提醒规则数量已达上限（%d）", maxAlertsPerUser)
	}
	name := strings.TrimSpace(in.Name)
	if allPositions {
		name = positionAlertAllName
	} else if q, e := s.market.GetQuote(ctx, market, symbol); e == nil && q.Name != "" {
		name = q.Name
	} else if errors.Is(e, datasource.ErrSymbolInvalid) {
		return nil, errors.New("无法识别的股票代码")
	}
	rule := &model.AlertRule{
		UserID: userID, Symbol: symbol, Market: market, Name: name,
		Kind: in.Kind, Op: in.Op, Threshold: round4(in.Threshold), Period: in.Period,
		Once: in.Once, Note: truncateRunes(strings.TrimSpace(in.Note), 250), Status: model.AlertStatusActive,
	}
	if err := common.DB.Create(rule).Error; err != nil {
		return nil, err
	}
	return rule, nil
}

// List 列出用户的提醒规则（可按状态过滤）。
func (s *AlertService) List(userID int64, status string) ([]model.AlertRule, error) {
	q := common.DB.Where("user_id = ?", userID)
	if status == model.AlertStatusActive || status == model.AlertStatusTriggered || status == model.AlertStatusPaused {
		q = q.Where("status = ?", status)
	}
	var rows []model.AlertRule
	err := q.Order("status = 'triggered' DESC, id DESC").Find(&rows).Error
	return rows, err
}

// alertCRUDLockWait CRUD（编辑/启停/删除）抢用户评估锁的最长等待。
//
// 后台单用户评估在整个轮次内持锁：预算最长 110s（交易时段）~3min（其余），推送 flush
// 还用 context.WithoutCancel 再占最多 alertNotifyFlushTimeout。CRUD 若无界共锁，用户在
// 交易时段点「暂停/删除提醒」会挂起两分钟；一旦浏览器/网关先断，mu.Lock(ctx) 返回裸
// context.Canceled 并被原样透传成英文 "context canceled"，而规则实际没改。
//
// 改为有界等待：拿不到锁就明确告知稍后重试，规则状态保持不变（不绕过锁，故评估期间
// 的一致性设计不变）。
const alertCRUDLockWait = 3 * time.Second

// lockAlertUserForCRUD 有界获取用户评估锁。成功返回已加锁的 lock，调用方负责 Unlock。
func lockAlertUserForCRUD(ctx context.Context, userID int64) (*alertUserLock, error) {
	mu := alertEvalLock(userID)
	wctx, cancel := context.WithTimeout(ctx, alertCRUDLockWait)
	defer cancel()
	if err := mu.Lock(wctx); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err() // 调用方自身已断开，保留原错误
		}
		return nil, errors.New("提醒规则正在后台评估中，请稍后重试")
	}
	return mu, nil
}

// Update 编辑规则（仅本人）。改动后若原为 triggered 则复位为 active 重新生效。
func (s *AlertService) Update(ctx context.Context, userID, id int64, in AlertInput) (*model.AlertRule, error) {
	mu, err := lockAlertUserForCRUD(ctx, userID)
	if err != nil {
		return nil, err
	}
	defer mu.Unlock()

	db := common.DB.WithContext(ctx)
	var rule model.AlertRule
	if err := db.Where("id = ? AND user_id = ?", id, userID).First(&rule).Error; err != nil {
		return nil, errors.New("提醒规则不存在")
	}
	if err := s.validate(&in); err != nil {
		return nil, err
	}
	// 未绑定 symbol 的规则（持仓类的「我的全部持仓」）不能被改成需要 symbol 的类型——
	// 那样评估时会拿空代码去拉行情，规则永久空转。改类型请新建一条。
	if rule.Symbol == "" && !isPositionAlertKind(in.Kind) {
		return nil, errors.New("「我的全部持仓」规则只能在持仓类提醒之间切换，改成其它类型请新建一条")
	}
	rule.Kind = in.Kind
	rule.Op = in.Op
	rule.Threshold = round4(in.Threshold)
	rule.Period = in.Period
	rule.Once = in.Once
	rule.Note = truncateRunes(strings.TrimSpace(in.Note), 250)
	rule.Status = model.AlertStatusActive
	rule.TriggeredAt = nil
	rule.TriggerMsg = ""
	if err := db.Save(&rule).Error; err != nil {
		return nil, err
	}
	return &rule, nil
}

// SetStatus 暂停/恢复规则（active <-> paused；恢复时清除命中标记）。
func (s *AlertService) SetStatus(ctx context.Context, userID, id int64, status string) (*model.AlertRule, error) {
	if status != model.AlertStatusActive && status != model.AlertStatusPaused {
		return nil, errors.New("非法状态")
	}
	mu, err := lockAlertUserForCRUD(ctx, userID)
	if err != nil {
		return nil, err
	}
	defer mu.Unlock()

	db := common.DB.WithContext(ctx)
	var rule model.AlertRule
	if err := db.Where("id = ? AND user_id = ?", id, userID).First(&rule).Error; err != nil {
		return nil, errors.New("提醒规则不存在")
	}
	rule.Status = status
	if status == model.AlertStatusActive {
		rule.TriggeredAt = nil
		rule.TriggerMsg = ""
	}
	if err := db.Save(&rule).Error; err != nil {
		return nil, err
	}
	return &rule, nil
}

// Delete 删除规则（仅本人）。其未读命中事件一并置为已忽略——
// 用户删规则即不再关注，不应残留在今日待办；历史事件保留可追溯。
func (s *AlertService) Delete(ctx context.Context, userID, id int64) error {
	mu, err := lockAlertUserForCRUD(ctx, userID)
	if err != nil {
		return err
	}
	defer mu.Unlock()

	return common.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		res := tx.Where("id = ? AND user_id = ?", id, userID).Delete(&model.AlertRule{})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return errors.New("提醒规则不存在")
		}
		return tx.Model(&model.AlertEvent{}).
			Where("rule_id = ? AND user_id = ? AND status = ?", id, userID, model.AlertEventUnread).
			Update("status", model.AlertEventDismissed).Error
	})
}

// TriggeredForUser 返回用户未读的命中事件（进入今日待办的提醒条目）。
// 批次 H 起以 alert_events 明细为准：每次命中（同日去重）落一条事件，
// 用户标记已读/忽略即从待办消失——替代旧的「按规则行 triggered_at 推断」口径。
func (s *AlertService) TriggeredForUser(userID int64) ([]model.AlertEvent, error) {
	var rows []model.AlertEvent
	err := common.DB.Where("user_id = ? AND status = ?", userID, model.AlertEventUnread).
		Order("triggered_at DESC").Limit(alertEventMaxList).Find(&rows).Error
	return rows, err
}

// --- 命中事件（明细/状态机） ---

// ListEvents 命中历史（可按状态过滤，倒序）。
func (s *AlertService) ListEvents(userID int64, status string, limit int) ([]model.AlertEvent, error) {
	if limit <= 0 || limit > alertEventMaxList {
		limit = 100
	}
	q := common.DB.Where("user_id = ?", userID)
	if status == model.AlertEventUnread || status == model.AlertEventRead || status == model.AlertEventDismissed {
		q = q.Where("status = ?", status)
	}
	var rows []model.AlertEvent
	err := q.Order("id DESC").Limit(limit).Find(&rows).Error
	return rows, err
}

// SetEventStatus 标记事件已读/忽略（也允许恢复未读，仅本人）。
func (s *AlertService) SetEventStatus(userID, id int64, status string) (*model.AlertEvent, error) {
	if status != model.AlertEventUnread && status != model.AlertEventRead && status != model.AlertEventDismissed {
		return nil, errors.New("非法状态")
	}
	var ev model.AlertEvent
	if err := common.DB.Where("id = ? AND user_id = ?", id, userID).First(&ev).Error; err != nil {
		return nil, errors.New("命中记录不存在")
	}
	ev.Status = status
	if err := common.DB.Save(&ev).Error; err != nil {
		return nil, err
	}
	return &ev, nil
}

// MarkAllEventsRead 全部未读事件标记已读，返回影响条数。
func (s *AlertService) MarkAllEventsRead(userID int64) (int64, error) {
	res := common.DB.Model(&model.AlertEvent{}).
		Where("user_id = ? AND status = ?", userID, model.AlertEventUnread).
		Update("status", model.AlertEventRead)
	return res.RowsAffected, res.Error
}

// alertUserLock 是可被 context 取消的用户级互斥。评估与规则 Update/SetStatus/Delete
// 共用同一把锁，保证这些操作返回后不会再由旧快照落事件或推送。
type alertUserLock struct {
	token chan struct{}
}

func newAlertUserLock() *alertUserLock {
	l := &alertUserLock{token: make(chan struct{}, 1)}
	l.token <- struct{}{}
	return l
}

func (l *alertUserLock) Lock(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-l.token:
		return nil
	}
}

func (l *alertUserLock) TryLock() bool {
	select {
	case <-l.token:
		return true
	default:
		return false
	}
}

func (l *alertUserLock) Unlock() {
	l.token <- struct{}{}
}

var alertEvalLocks sync.Map // int64 -> *alertUserLock

// alertRuleEvalCursors 记录每个用户最后一条已开始尝试的行情提醒规则。值只在对应
// 用户锁内更新；sync.Map 让规则 CRUD 与测试清理可安全跨 goroutine 访问。
var alertRuleEvalCursors sync.Map // int64 -> int64

var errAlertEvaluationBusy = errors.New("上一轮提醒评估仍在执行")

func alertEvalLock(userID int64) *alertUserLock {
	m, _ := alertEvalLocks.LoadOrStore(userID, newAlertUserLock())
	return m.(*alertUserLock)
}

// rotateAlertRulesAfterID 的输入由数据库按 id ASC 返回。从 lastID 的下一条开始，
// lastID 已删除/暂停时从首个更大 ID 接续，超过末尾则回绕。
func rotateAlertRulesAfterID(rules []model.AlertRule, lastID int64) []model.AlertRule {
	if len(rules) < 2 || lastID <= 0 {
		return rules
	}
	start := 0
	for i := range rules {
		if rules[i].ID > lastID {
			start = i
			break
		}
	}
	if start == 0 {
		return rules
	}
	rotated := make([]model.AlertRule, 0, len(rules))
	rotated = append(rotated, rules[start:]...)
	rotated = append(rotated, rules[:start]...)
	return rotated
}

// walkAlertRules 从用户游标的下一条开始执行。规则进入 attempt 前即记为“已尝试”，
// 即使慢源耗尽本轮 context，下轮也从后一条开始；尚未进入循环的后缀规则不推进。
func walkAlertRules(ctx context.Context, userID int64, rules []model.AlertRule,
	attempt func(model.AlertRule) error) error {
	var lastID int64
	if v, ok := alertRuleEvalCursors.Load(userID); ok {
		lastID, _ = v.(int64)
	}
	for _, rule := range rotateAlertRulesAfterID(rules, lastID) {
		if err := ctx.Err(); err != nil {
			return err
		}
		alertRuleEvalCursors.Store(userID, rule.ID)
		if err := attempt(rule); err != nil {
			return err
		}
	}
	return nil
}

type alertPersistResult struct {
	active       bool
	eventCreated bool
}

var errAlertRuleChanged = errors.New("提醒规则已不再生效")

// persistAlertEvaluation 将事件和规则最近状态放在同一事务。命中前重新读取 active 行，
// 即使未来扩成多实例，条件更新失败也会回滚事件，不留下孤儿事件或半写状态。
func persistAlertEvaluation(ctx context.Context, rule model.AlertRule, value float64, triggered bool,
	msg, today string, now time.Time) (alertPersistResult, error) {
	var out alertPersistResult
	err := common.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var current model.AlertRule
		q := tx.Where("id = ? AND user_id = ? AND status = ?", rule.ID, rule.UserID, model.AlertStatusActive)
		// SQLite 不支持 SELECT ... FOR UPDATE；MySQL 生产环境用行锁串行化同一规则的
		// 多实例评估，避免两个实例同时读到旧 triggered_at 后各插一条事件。
		if tx.Dialector.Name() != "sqlite" {
			q = q.Clauses(clause.Locking{Strength: "UPDATE"})
		}
		err := q.First(&current).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return errAlertRuleChanged
		}
		if err != nil {
			return err
		}
		out.active = true
		updates := map[string]any{"last_value": round2(value), "last_check_date": today}
		if triggered {
			updates["triggered_at"] = now
			updates["trigger_msg"] = truncateRunes(msg, 256)
			if current.Once {
				updates["status"] = model.AlertStatusTriggered
			}
			alreadyToday := current.TriggeredAt != nil &&
				current.TriggeredAt.In(time.Local).Format("2006-01-02") == today
			if !alreadyToday {
				ev := model.AlertEvent{
					RuleID: current.ID, UserID: current.UserID,
					Symbol: current.Symbol, Market: current.Market, Name: current.Name, Kind: current.Kind,
					Message: truncateRunes(msg, 256), TradeDate: today,
					TriggeredAt: now, Status: model.AlertEventUnread,
				}
				if err := tx.Create(&ev).Error; err != nil {
					return err
				}
				out.eventCreated = true
			}
		}
		res := tx.Model(&model.AlertRule{}).
			Where("id = ? AND user_id = ? AND status = ?", current.ID, current.UserID, model.AlertStatusActive).
			Updates(updates)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected != 1 {
			return errAlertRuleChanged
		}
		return nil
	})
	if errors.Is(err, errAlertRuleChanged) {
		return alertPersistResult{}, nil
	}
	return out, err
}

func alertNotificationsEnabled(ctx context.Context, userID int64) (bool, error) {
	db := common.DB.WithContext(ctx)
	var cnt int64
	if err := db.Model(&model.NotifyChannel{}).
		Where("user_id = ? AND enabled = ?", userID, true).Count(&cnt).Error; err != nil {
		return false, err
	}
	if cnt == 0 {
		return false, nil
	}
	var pref model.UserPreference
	if err := db.Where("user_id = ?", userID).First(&pref).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return false, nil
		}
		return false, err
	}
	return pref.EnableNotify, nil
}

// flushAlertNotification 在评估返回前同步尝试推送。WithoutCancel 保留调用链上的值，
// 但不继承已经触发的取消/deadline；外层总超时保证用户锁不会被通知网络无限占用。
func flushAlertNotification(sourceCtx context.Context, notifier alertNotifier, userID int64,
	title, kind string, lines []string) {
	if notifier == nil || len(lines) == 0 {
		return
	}
	base := context.Background()
	if sourceCtx != nil {
		base = context.WithoutCancel(sourceCtx)
	}
	ctx, cancel := context.WithTimeout(base, alertNotifyFlushTimeout)
	defer cancel()
	notifier.SendMsgContext(ctx, userID, NotifyMessage{
		Title: title, Content: strings.Join(lines, "\n"),
		Route: "/alerts", Kind: kind,
	})
}

// --- 评估编排 ---

// EvaluateUser 手动「立即检查」入口：行情类 + 财报日历类全量评估。
// 财报类查本地表（零上游成本），让用户手动检查能当场验证财报提醒。
func (s *AlertService) EvaluateUser(ctx context.Context, userID int64) (int, error) {
	hits, err := s.evaluateUserMarket(ctx, userID)
	if err != nil {
		return 0, err
	}
	earnHits, err := s.evaluateEarnRulesForUserContext(ctx, userID)
	if err != nil {
		return hits, err
	}
	hits += earnHits
	return hits, nil
}

// evaluateUserMarket 盘中口径：只评行情类规则，显式排除财报日历类 kind——
// 否则高频轮询会把财报规则拉去每 symbol 拉行情空转（财报类由财报刷新 job 每日一评）。
func (s *AlertService) evaluateUserMarket(ctx context.Context, userID int64) (int, error) {
	mu := alertEvalLock(userID)
	if err := mu.Lock(ctx); err != nil {
		return 0, err
	}
	defer mu.Unlock()
	return s.evaluateUserMarketUnlocked(ctx, userID)
}

// tryEvaluateUserMarket 供后台固定节拍调度使用。上一轮或手动检查仍占用该用户锁时
// 直接跳过，避免慢上游让 goroutine 排队并在恢复后集中重复评估。
func (s *AlertService) tryEvaluateUserMarket(ctx context.Context, userID int64) (int, error) {
	mu := alertEvalLock(userID)
	if !mu.TryLock() {
		return 0, errAlertEvaluationBusy
	}
	defer mu.Unlock()
	return s.evaluateUserMarketUnlocked(ctx, userID)
}

func (s *AlertService) evaluateUserMarketUnlocked(ctx context.Context, userID int64) (int, error) {
	var rules []model.AlertRule
	if err := common.DB.WithContext(ctx).Where("user_id = ? AND status = ? AND kind NOT IN ?",
		userID, model.AlertStatusActive, nonPositionMarketKinds).Order("id ASC").Find(&rules).Error; err != nil {
		return 0, err
	}
	hits, err := s.evaluateRules(ctx, rules)
	if err != nil {
		return hits, err
	}
	// 持仓卖出决策类走独立编排（评估单元是持仓，不是 symbol）。
	var posRules []model.AlertRule
	if err := common.DB.WithContext(ctx).Where("user_id = ? AND status = ? AND kind IN ?",
		userID, model.AlertStatusActive, positionAlertKinds).Order("id ASC").Find(&posRules).Error; err != nil {
		return hits, err
	}
	posHits, err := s.evaluatePositionRules(ctx, userID, posRules)
	hits += posHits
	return hits, err
}

// evaluateRules 评估一批规则，按 symbol 缓存行情/日线/估值，避免重复请求。
func (s *AlertService) evaluateRules(ctx context.Context, rules []model.AlertRule) (int, error) {
	type md struct {
		eval     alertEval
		ok       bool
		valTried bool // 估值源振幅是否已尝试拉取（失败不重试）
	}
	cache := map[string]md{}
	today := time.Now().In(time.Local).Format("2006-01-02")
	hits := 0

	// fillBars 组装 ma/breakout/volume_surge 的日线序列。breakout 的「前高/前低」与
	// volume_surge 的「均量」窗口剔除当日在途 bar（否则当日 high 与含自身的窗口比较
	// 恒成立、当日量参与均量会稀释放量信号）；MA 序列保留当日（现价参与均线是常用口径）。
	fillBars := func(e *alertEval, bars []datasource.Bar) {
		for _, b := range bars {
			e.Closes = append(e.Closes, b.Close)
			if b.TradeDate != today {
				e.Highs = append(e.Highs, b.High)
				e.Lows = append(e.Lows, b.Low)
				e.Volumes = append(e.Volumes, b.Volume)
			}
		}
	}

	// 推送开关：用户配置了启用的通道，且偏好「开启提醒」打开（一批规则同属一个用户，只查一次）。
	notifyOn := false
	if s.notify != nil && len(rules) > 0 {
		var err error
		notifyOn, err = alertNotificationsEnabled(ctx, rules[0].UserID)
		if err != nil {
			return 0, err
		}
	}
	var pushLines []string
	if notifyOn {
		uid := rules[0].UserID
		// 仅注册一次；无论正常结束还是后续规则超时，已落库事件都会在返回前尝试推送。
		defer func() {
			flushAlertNotification(ctx, s.notify, uid, "QuantVista 提醒命中", NotifyMsgKindAlert, pushLines)
		}()
	}

	if len(rules) == 0 {
		return 0, nil
	}
	userID := rules[0].UserID
	err := walkAlertRules(ctx, userID, rules, func(rule model.AlertRule) error {
		key := QuoteKey(rule.Market, rule.Symbol)
		data, cached := cache[key]
		if !cached {
			data = md{}
			// fail-closed：走新鲜行情链路（主源旧价不当成功，逐源找当前有效行情），
			// 非 fresh（stale/unknown）时本轮跳过该标的——旧价触发条件提醒是误报（如
			// 昨日价击穿今日已回升的阈值），宁可这轮不评也不拿旧价评。
			q, fi, quoteErr := s.market.GetFreshQuote(ctx, rule.Market, rule.Symbol)
			if quoteErr == nil && q != nil && q.Price > 0 && fi.Status == freshStatusFresh {
				data.eval = alertEval{Price: q.Price, DayHigh: q.High, DayLow: q.Low, ChangePct: q.ChangePct, DayVolume: q.Volume}
				// 振幅回退基线：(high-low)/prev_close（估值源自带值优先，见下方按需覆盖）。
				if q.PrevClose > 0 && q.High > 0 && q.High >= q.Low {
					data.eval.Amplitude = round2((q.High - q.Low) / q.PrevClose * 100)
				}
				data.ok = true
				// ma/breakout/volume_surge 才需要日线。
				if alertKindNeedsBars(rule.Kind) {
					if bars, berr := s.market.GetDailyBars(ctx, rule.Market, rule.Symbol, alertBarLimit); berr == nil {
						fillBars(&data.eval, bars)
					}
				}
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			cache[key] = data
		}
		if !data.ok {
			return nil
		}
		// 若已缓存但缺日线而本规则需要，补拉一次。
		if alertKindNeedsBars(rule.Kind) && len(data.eval.Closes) == 0 {
			if bars, berr := s.market.GetDailyBars(ctx, rule.Market, rule.Symbol, alertBarLimit); berr == nil {
				fillBars(&data.eval, bars)
				cache[key] = data
			}
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		// 振幅规则优先用估值源自带振幅（腾讯行情串），每 symbol 只试一次，失败保留 quote 回退值。
		// fail-closed：估值 DataTime 须为 fresh 才允许覆盖——fresh quote 推导的振幅被
		// 过期估值（昨日/停滞口径）覆盖，等于把已验证的新数据换成旧数据。
		if rule.Kind == model.AlertKindAmplitude && !data.valTried {
			data.valTried = true
			if v, verr := s.market.GetValuation(ctx, rule.Market, rule.Symbol); verr == nil && v.Amplitude > 0 &&
				s.market.QuoteFreshnessOf(rule.Market, v.DataTime).Status == freshStatusFresh {
				data.eval.Amplitude = v.Amplitude
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			cache[key] = data
		}

		if err := ctx.Err(); err != nil {
			return err
		}
		triggered, value, msg := evaluateAlert(rule, data.eval)
		now := time.Now()
		persisted, err := persistAlertEvaluation(ctx, rule, value, triggered, msg, today, now)
		if err != nil {
			return err
		}
		if triggered && persisted.active {
			hits++
		}
		if triggered && notifyOn && persisted.eventCreated {
			name := rule.Name
			if name == "" {
				name = rule.Symbol
			}
			pushLines = append(pushLines, name+"("+rule.Symbol+")："+msg)
		}
		return nil
	})
	if err != nil {
		return hits, err
	}
	return hits, nil
}

const (
	alertIntervalTrading  = 2 * time.Minute
	alertIntervalIdle     = 30 * time.Minute
	alertTimeoutTrading   = 110 * time.Second
	alertTimeoutIdle      = 3 * time.Minute
	alertWindowGuard      = 5 * time.Second
	alertMaxConcurrentUsr = 8
)

// alertIntervalAt 返回当前时段的评估频率（纯函数，交易日判定由调用方复用统一日历口径）。
func alertIntervalAt(now time.Time, tradingDay bool) time.Duration {
	minute := now.Hour()*60 + now.Minute()
	if tradingDay && ((minute >= 9*60+25 && minute <= 11*60+35) ||
		(minute >= 12*60+55 && minute <= 15*60+5)) {
		return alertIntervalTrading
	}
	return alertIntervalIdle
}

// alertNextDelayAt 在休息时段仍会准点唤醒到下一交易窗口，避免 09:24 启动后按
// 30 分钟空闲间隔睡到 09:54，破坏「盘中最多约 2 分钟」的提醒语义。
func alertNextDelayAt(now time.Time, tradingDay bool) time.Duration {
	delay := alertIntervalAt(now, tradingDay)
	if !tradingDay || delay == alertIntervalTrading {
		return delay
	}
	if start, ok := alertNextTradingWindow(now); ok && start.Sub(now) < delay {
		return start.Sub(now)
	}
	return delay
}

func alertNextTradingWindow(now time.Time) (time.Time, bool) {
	for _, hm := range [][2]int{{9, 25}, {12, 55}} {
		start := time.Date(now.Year(), now.Month(), now.Day(), hm[0], hm[1], 0, 0, now.Location())
		if now.Before(start) {
			return start, true
		}
	}
	return time.Time{}, false
}

func alertEvaluationTimeoutAt(now time.Time, tradingDay bool) time.Duration {
	if alertIntervalAt(now, tradingDay) == alertIntervalTrading {
		return alertTimeoutTrading
	}
	// 服务刚好在开盘前启动时，空闲轮必须在交易窗口前释放用户锁；剩余不足
	// guard 时直接不启动该轮，让窗口开始的盘中轮优先。
	if tradingDay {
		if start, ok := alertNextTradingWindow(now); ok {
			budget := start.Sub(now) - alertWindowGuard
			if budget <= 0 {
				return 0
			}
			if budget < alertTimeoutIdle {
				return budget
			}
		}
	}
	return alertTimeoutIdle
}

type alertUserEvaluator func(context.Context, int64) (int, error)

// evaluateAlertUsers 用固定 worker 数消费用户列表。dispatchCtx 只限制本轮还能否派发
// 新用户；已被 worker 接收的用户使用从接收时开始计算的独立预算，不继承派发取消。
// 返回值是实际进入 worker 并开始调用 evaluate 的用户数。
func evaluateAlertUsers(dispatchCtx context.Context, userIDs []int64, maxConcurrent int,
	userTimeout time.Duration, evaluate alertUserEvaluator) int {
	if len(userIDs) == 0 {
		return 0
	}
	if dispatchCtx == nil {
		dispatchCtx = context.Background()
	}
	if maxConcurrent <= 0 {
		maxConcurrent = 1
	}
	if maxConcurrent > len(userIDs) {
		maxConcurrent = len(userIDs)
	}
	jobs := make(chan int64)
	var wg sync.WaitGroup
	dispatched := 0
	userBaseCtx := context.WithoutCancel(dispatchCtx)
	worker := func() {
		defer wg.Done()
		for uid := range jobs {
			var userCtx context.Context
			var cancel context.CancelFunc
			if userTimeout > 0 {
				userCtx, cancel = context.WithTimeout(userBaseCtx, userTimeout)
			} else {
				userCtx, cancel = context.WithCancel(userBaseCtx)
			}
			n, err := evaluate(userCtx, uid)
			cancel()
			if errors.Is(err, errAlertEvaluationBusy) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				continue
			}
			if err != nil {
				common.SysWarn("评估用户 %d 提醒失败: %v", uid, err)
			} else if n > 0 {
				common.SysLog("用户 %d 提醒命中 %d 条", uid, n)
			}
		}
	}
	wg.Add(maxConcurrent)
	for i := 0; i < maxConcurrent; i++ {
		go worker()
	}

sendLoop:
	for _, uid := range userIDs {
		if dispatchCtx.Err() != nil {
			break sendLoop
		}
		select {
		case jobs <- uid:
			// 无缓冲通道发送成功即表示某个 worker 已接收；该用户从此拥有独立
			// timeout，即使派发窗口恰在此后关闭也必须执行，保证已派发集合是连续前缀。
			dispatched++
		case <-dispatchCtx.Done():
			break sendLoop
		}
	}
	close(jobs)
	wg.Wait()
	return dispatched
}

var alertRoundRunning atomic.Bool
var alertRoundCursor atomic.Uint64

// rotateAlertUserIDs 让受整轮预算影响而未执行的后排用户在下一轮前移。
// 输入来自按 user_id 排序的查询，故跨轮顺序稳定且可公平轮转。
func rotateAlertUserIDs(userIDs []int64, start int) []int64 {
	if len(userIDs) == 0 {
		return userIDs
	}
	start %= len(userIDs)
	if start < 0 {
		start += len(userIDs)
	}
	if start == 0 {
		return userIDs
	}
	rotated := make([]int64, 0, len(userIDs))
	rotated = append(rotated, userIDs[start:]...)
	rotated = append(rotated, userIDs[:start]...)
	return rotated
}

// startAlertRound 只负责启动一轮，立即返回给调度器开始计算下一次 timer。整轮防重入，
// 避免数据库或上游异常时多个 2 分钟轮次叠加。
func startAlertRound(now time.Time, svc *AlertService) {
	if common.DB == nil || svc == nil {
		return
	}
	tradingDay := isTradingDayToday(now)
	dispatchTimeout := alertEvaluationTimeoutAt(now, tradingDay)
	if dispatchTimeout <= 0 || !alertRoundRunning.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer alertRoundRunning.Store(false)
		dispatchCtx, cancel := context.WithTimeout(context.Background(), dispatchTimeout)
		defer cancel()

		var userIDs []int64
		if err := common.DB.WithContext(dispatchCtx).Model(&model.AlertRule{}).
			Where("status = ? AND kind NOT IN ?", model.AlertStatusActive, earnAlertKinds).
			Distinct().Order("user_id ASC").Pluck("user_id", &userIDs).Error; err != nil {
			if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
				common.SysWarn("提醒评估列举用户失败: %v", err)
			}
			return
		}
		if len(userIDs) > 0 {
			cursor := alertRoundCursor.Load()
			userIDs = rotateAlertUserIDs(userIDs, int(cursor%uint64(len(userIDs))))
		}
		dispatched := evaluateAlertUsers(dispatchCtx, userIDs, alertMaxConcurrentUsr,
			dispatchTimeout, svc.tryEvaluateUserMarket)
		if dispatched > 0 {
			alertRoundCursor.Add(uint64(dispatched))
		}
		if dispatched < len(userIDs) {
			common.SysWarn("提醒评估容量不足: 本轮实际派发 %d/%d 个用户，并发=%d，派发窗口=%s，单用户预算=%s；未派发用户下轮优先",
				dispatched, len(userIDs), alertMaxConcurrentUsr, dispatchTimeout, dispatchTimeout)
		}
	}()
}

// StartAlertJobs 后台评估提醒：启动延迟 60s 一次；交易时段 2 分钟、其余 30 分钟，
// 遍历有 active 规则的用户。
// 只做行情类（财报日历类由 service/finance.go 的每日 job 调 EvaluateEarningsAll）。
func StartAlertJobs(mgr *datasource.Manager) {
	svc := NewAlertService(NewMarketService(mgr))
	go func() {
		time.Sleep(60 * time.Second)
		startAlertRound(time.Now().In(time.Local), svc)
		for {
			now := time.Now().In(time.Local)
			delay := alertNextDelayAt(now, isTradingDayToday(now))
			t := time.NewTimer(delay)
			<-t.C
			startAlertRound(time.Now().In(time.Local), svc)
		}
	}()
}

// --- 财报日历类评估（earn_date / earn_fcst，每日一评） ---

// evaluateEarnDate 纯函数：距预约披露日 ≤N 自然日（N=threshold）命中。
// 防重（Once=false 时窗口内每天评估都会满足条件）：本披露窗口内已提醒过
// （TriggeredAt 晚于「披露日-N 天」）则不再命中。返回（命中、观测值=剩余天数、说明）。
func evaluateEarnDate(rule model.AlertRule, appointDate, reportTypeName string, now time.Time) (bool, float64, string) {
	ad, err := time.ParseInLocation("2006-01-02", appointDate, time.Local)
	if err != nil {
		return false, 0, ""
	}
	n := int(rule.Threshold)
	if n <= 0 {
		n = 3
	}
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.Local)
	days := int(ad.Sub(today).Hours() / 24)
	if days < 0 || days > n {
		return false, float64(days), ""
	}
	windowStart := ad.AddDate(0, 0, -n)
	if rule.TriggeredAt != nil && !rule.TriggeredAt.Before(windowStart) {
		return false, float64(days), "" // 本窗口已提醒过
	}
	label := reportTypeName
	if label == "" {
		label = "财报"
	}
	when := fmt.Sprintf("%d 天后", days)
	if days == 0 {
		when = "今日"
	}
	return true, float64(days), fmt.Sprintf("将于 %s 披露 %s（%s）", ad.Format("01-02"), label, when)
}

// evaluateEarnFcst 纯函数：出现新业绩预告（发布日在近 forecastFreshDays 自然日内）命中。
// 防重：该预告发布日当天或之后已提醒过（TriggeredAt 日期 >= NoticeDate）则不再命中——
// 同一份预告只提醒一次，新报告期的新预告（更新的 NoticeDate）会再次命中。
func evaluateEarnFcst(rule model.AlertRule, fc *model.EarningsForecast, now time.Time) (bool, float64, string) {
	if fc == nil || fc.NoticeDate == "" {
		return false, 0, ""
	}
	nd, err := time.ParseInLocation("2006-01-02", fc.NoticeDate, time.Local)
	if err != nil {
		return false, 0, ""
	}
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.Local)
	age := int(today.Sub(nd).Hours() / 24)
	if age < 0 || age > forecastFreshDays {
		return false, 0, ""
	}
	if rule.TriggeredAt != nil && rule.TriggeredAt.In(time.Local).Format("2006-01-02") >= fc.NoticeDate {
		return false, 0, "" // 这份预告已提醒过
	}
	typ := fc.PredictType
	if typ == "" {
		typ = "业绩预告"
	}
	detail := ""
	switch {
	case fc.AmpLower != 0 || fc.AmpUpper != 0:
		detail = fmt.Sprintf("，%s变动 %.2f%%~%.2f%%", fc.PredictFinance, fc.AmpLower, fc.AmpUpper)
	case fc.Content != "":
		detail = "：" + truncateRunes(fc.Content, 60)
	}
	return true, fc.AmpLower, fmt.Sprintf("%s 发布业绩预告【%s】%s", fc.NoticeDate, typ, detail)
}

// evaluateEarnRulesForUser 保留每日财报 job 的简洁入口；真实实现同样绑定 context、
// 用户锁和事务写入，不与盘中或手动评估交叉。
func (s *AlertService) evaluateEarnRulesForUser(userID int64) int {
	n, err := s.evaluateEarnRulesForUserContext(context.Background(), userID)
	if err != nil {
		common.SysWarn("评估用户 %d 财报提醒失败: %v", userID, err)
	}
	return n
}

func (s *AlertService) evaluateEarnRulesForUserContext(ctx context.Context, userID int64) (int, error) {
	mu := alertEvalLock(userID)
	if err := mu.Lock(ctx); err != nil {
		return 0, err
	}
	defer mu.Unlock()
	return s.evaluateEarnRulesForUserUnlocked(ctx, userID)
}

func (s *AlertService) evaluateEarnRulesForUserUnlocked(ctx context.Context, userID int64) (int, error) {
	db := common.DB.WithContext(ctx)
	var rules []model.AlertRule
	if err := db.Where("user_id = ? AND status = ? AND kind IN ?",
		userID, model.AlertStatusActive, earnAlertKinds).Order("id ASC").Find(&rules).Error; err != nil {
		return 0, err
	}
	if len(rules) == 0 {
		return 0, nil
	}
	now := time.Now()
	today := now.In(time.Local).Format("2006-01-02")
	notifyOn := false
	if s.notify != nil {
		var err error
		notifyOn, err = alertNotificationsEnabled(ctx, userID)
		if err != nil {
			return 0, err
		}
	}
	var pushLines []string
	if notifyOn {
		// 与行情提醒一致：批内后续规则失败/超时也不能吞掉已落库事件的推送尝试。
		defer func() {
			flushAlertNotification(ctx, s.notify, userID, "QuantVista 财报提醒", NotifyMsgKindEarn, pushLines)
		}()
	}
	hits := 0

	for _, rule := range rules {
		if err := ctx.Err(); err != nil {
			return hits, err
		}
		var triggered bool
		var value float64
		var msg string
		switch rule.Kind {
		case model.AlertKindEarnDate:
			var sched model.DisclosureSchedule
			err := db.Where("symbol = ? AND appoint_date >= ? AND is_published = ?", rule.Symbol, today, false).
				Order("appoint_date ASC").First(&sched).Error
			if err == nil {
				triggered, value, msg = evaluateEarnDate(rule, sched.AppointDate, sched.ReportTypeName, now)
			} else if !errors.Is(err, gorm.ErrRecordNotFound) {
				return hits, err
			}
		case model.AlertKindEarnFcst:
			var forecast model.EarningsForecast
			err := db.Where("symbol = ?", rule.Symbol).Order("notice_date DESC, id DESC").First(&forecast).Error
			if err == nil {
				triggered, value, msg = evaluateEarnFcst(rule, &forecast, now)
			} else if !errors.Is(err, gorm.ErrRecordNotFound) {
				return hits, err
			}
		}

		persisted, err := persistAlertEvaluation(ctx, rule, value, triggered, msg, today, time.Now())
		if err != nil {
			return hits, err
		}
		if triggered && persisted.active {
			hits++
		}
		if triggered && notifyOn && persisted.eventCreated {
			name := rule.Name
			if name == "" {
				name = rule.Symbol
			}
			pushLines = append(pushLines, name+"("+rule.Symbol+")："+msg)
		}
	}

	return hits, nil
}

// EvaluateEarningsAll 财报类提醒每日一评：遍历有 active 财报规则的用户。
// 由 service/finance.go 的每日刷新 job 在数据更新后调用（数据新才评，避免旧数据空评）。
func (s *AlertService) EvaluateEarningsAll() int {
	if common.DB == nil {
		return 0
	}
	var userIDs []int64
	if err := common.DB.Model(&model.AlertRule{}).
		Where("status = ? AND kind IN ?", model.AlertStatusActive, earnAlertKinds).
		Distinct().Pluck("user_id", &userIDs).Error; err != nil {
		common.SysWarn("财报提醒列举用户失败: %v", err)
		return 0
	}
	total := 0
	for _, uid := range userIDs {
		total += s.evaluateEarnRulesForUser(uid)
	}
	return total
}
