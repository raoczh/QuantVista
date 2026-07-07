package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"quantvista/common"
	"quantvista/datasource"
	"quantvista/model"
)

// AlertService 条件提醒：管理规则 + 按行情/日线评估命中（命中判定用当日 OHLC 避免漏判盘中触达）。
// 命中默认仅落库供待办/页面高亮；若用户配置了启用的推送通道，则额外主动推送（同日去重）。
type AlertService struct {
	market *MarketService
	notify *NotifyService
}

func NewAlertService(market *MarketService) *AlertService {
	return &AlertService{market: market, notify: NewNotifyService()}
}

const (
	alertBarLimit     = 90  // 评估 ma/breakout/volume_surge 需要的日线条数
	maxAlertsPerUser  = 200
	volumeAvgWindow   = 20  // 放量判定的均量窗口（交易日）
	alertEventMaxList = 500 // 命中历史单次最多返回条数
)

var validAlertKind = map[string]bool{
	model.AlertKindPrice:       true,
	model.AlertKindPctChange:   true,
	model.AlertKindMA:          true,
	model.AlertKindBreakout:    true,
	model.AlertKindVolumeSurge: true,
	model.AlertKindAmplitude:   true,
	model.AlertKindEarnDate:    true,
	model.AlertKindEarnFcst:    true,
}

// earnAlertKinds 财报日历类 kind：盘中 15min 行情评估显式排除（否则会被拉去每
// symbol 拉行情空转），由财报刷新 job 每日一评 + 手动「立即检查」时顺带评估（查本地表零上游成本）。
var earnAlertKinds = []string{model.AlertKindEarnDate, model.AlertKindEarnFcst}

func isEarnAlertKind(kind string) bool {
	return kind == model.AlertKindEarnDate || kind == model.AlertKindEarnFcst
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
	}
	return nil
}

// Create 新建提醒规则（校验代码 + 取名）。
func (s *AlertService) Create(ctx context.Context, userID int64, in AlertInput) (*model.AlertRule, error) {
	symbol, market, err := normalizeSymbolMarket(in.Symbol, in.Market)
	if err != nil {
		return nil, err
	}
	if err := s.validate(&in); err != nil {
		return nil, err
	}
	var cnt int64
	common.DB.Model(&model.AlertRule{}).Where("user_id = ?", userID).Count(&cnt)
	if cnt >= maxAlertsPerUser {
		return nil, fmt.Errorf("提醒规则数量已达上限（%d）", maxAlertsPerUser)
	}
	name := strings.TrimSpace(in.Name)
	if q, e := s.market.GetQuote(ctx, market, symbol); e == nil && q.Name != "" {
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

// Update 编辑规则（仅本人）。改动后若原为 triggered 则复位为 active 重新生效。
func (s *AlertService) Update(userID, id int64, in AlertInput) (*model.AlertRule, error) {
	var rule model.AlertRule
	if err := common.DB.Where("id = ? AND user_id = ?", id, userID).First(&rule).Error; err != nil {
		return nil, errors.New("提醒规则不存在")
	}
	if err := s.validate(&in); err != nil {
		return nil, err
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
	if err := common.DB.Save(&rule).Error; err != nil {
		return nil, err
	}
	return &rule, nil
}

// SetStatus 暂停/恢复规则（active <-> paused；恢复时清除命中标记）。
func (s *AlertService) SetStatus(userID, id int64, status string) (*model.AlertRule, error) {
	if status != model.AlertStatusActive && status != model.AlertStatusPaused {
		return nil, errors.New("非法状态")
	}
	var rule model.AlertRule
	if err := common.DB.Where("id = ? AND user_id = ?", id, userID).First(&rule).Error; err != nil {
		return nil, errors.New("提醒规则不存在")
	}
	rule.Status = status
	if status == model.AlertStatusActive {
		rule.TriggeredAt = nil
		rule.TriggerMsg = ""
	}
	if err := common.DB.Save(&rule).Error; err != nil {
		return nil, err
	}
	return &rule, nil
}

// Delete 删除规则（仅本人）。其未读命中事件一并置为已忽略——
// 用户删规则即不再关注，不应残留在今日待办；历史事件保留可追溯。
func (s *AlertService) Delete(userID, id int64) error {
	res := common.DB.Where("id = ? AND user_id = ?", id, userID).Delete(&model.AlertRule{})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return errors.New("提醒规则不存在")
	}
	common.DB.Model(&model.AlertEvent{}).
		Where("rule_id = ? AND user_id = ? AND status = ?", id, userID, model.AlertEventUnread).
		Update("status", model.AlertEventDismissed)
	return nil
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

// recordEvent 命中时落明细事件。同日去重与推送同口径：该规则今天已命中过
//（旧 triggered_at 为今天）则不重复落——手动「立即检查」一日多轮不会刷屏。
func recordEvent(rule model.AlertRule, msg, today string, now time.Time) bool {
	if rule.TriggeredAt != nil && rule.TriggeredAt.In(time.Local).Format("2006-01-02") == today {
		return false
	}
	ev := model.AlertEvent{
		RuleID: rule.ID, UserID: rule.UserID,
		Symbol: rule.Symbol, Market: rule.Market, Name: rule.Name, Kind: rule.Kind,
		Message: truncateRunes(msg, 256), TriggeredAt: now, Status: model.AlertEventUnread,
	}
	if err := common.DB.Create(&ev).Error; err != nil {
		common.SysWarn("落提醒命中事件失败: %v", err)
		return false
	}
	return true
}

// --- 评估编排 ---

// EvaluateUser 手动「立即检查」入口：行情类 + 财报日历类全量评估。
// 财报类查本地表（零上游成本），让用户手动检查能当场验证财报提醒。
func (s *AlertService) EvaluateUser(ctx context.Context, userID int64) (int, error) {
	hits, err := s.evaluateUserMarket(ctx, userID)
	if err != nil {
		return 0, err
	}
	hits += s.evaluateEarnRulesForUser(userID)
	return hits, nil
}

// evaluateUserMarket 盘中口径：只评行情类规则，显式排除财报日历类 kind——
// 否则 15min 一轮会被财报规则拉去每 symbol 拉行情空转（财报类由财报刷新 job 每日一评）。
func (s *AlertService) evaluateUserMarket(ctx context.Context, userID int64) (int, error) {
	var rules []model.AlertRule
	if err := common.DB.Where("user_id = ? AND status = ? AND kind NOT IN ?",
		userID, model.AlertStatusActive, earnAlertKinds).Find(&rules).Error; err != nil {
		return 0, err
	}
	return s.evaluateRules(ctx, rules), nil
}

// evaluateRules 评估一批规则，按 symbol 缓存行情/日线/估值，避免重复请求。
func (s *AlertService) evaluateRules(ctx context.Context, rules []model.AlertRule) int {
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
		notifyOn = s.notify.HasEnabledChannel(rules[0].UserID) && userNotifyEnabled(rules[0].UserID)
	}
	var pushLines []string

	for _, rule := range rules {
		key := QuoteKey(rule.Market, rule.Symbol)
		data, cached := cache[key]
		if !cached {
			data = md{}
			if q, err := s.market.GetQuote(ctx, rule.Market, rule.Symbol); err == nil && q.Price > 0 {
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
			cache[key] = data
		}
		if !data.ok {
			continue
		}
		// 若已缓存但缺日线而本规则需要，补拉一次。
		if alertKindNeedsBars(rule.Kind) && len(data.eval.Closes) == 0 {
			if bars, berr := s.market.GetDailyBars(ctx, rule.Market, rule.Symbol, alertBarLimit); berr == nil {
				fillBars(&data.eval, bars)
				cache[key] = data
			}
		}
		// 振幅规则优先用估值源自带振幅（腾讯行情串），每 symbol 只试一次，失败保留 quote 回退值。
		if rule.Kind == model.AlertKindAmplitude && !data.valTried {
			data.valTried = true
			if v, verr := s.market.GetValuation(ctx, rule.Market, rule.Symbol); verr == nil && v.Amplitude > 0 {
				data.eval.Amplitude = v.Amplitude
			}
			cache[key] = data
		}

		triggered, value, msg := evaluateAlert(rule, data.eval)
		updates := map[string]any{"last_value": round2(value), "last_check_date": today}
		if triggered {
			hits++
			now := time.Now()
			updates["triggered_at"] = &now
			updates["trigger_msg"] = truncateRunes(msg, 256)
			if rule.Once {
				updates["status"] = model.AlertStatusTriggered
			}
			// 命中明细落库（同日去重，用旧 triggered_at 判断——须在规则行更新前）。
			recordEvent(rule, msg, today, now)
			// 同日去重：本规则今天尚未命中过才纳入推送。
			if notifyOn && (rule.TriggeredAt == nil || rule.TriggeredAt.In(time.Local).Format("2006-01-02") != today) {
				name := rule.Name
				if name == "" {
					name = rule.Symbol
				}
				pushLines = append(pushLines, name+"("+rule.Symbol+")："+msg)
			}
		}
		common.DB.Model(&model.AlertRule{}).Where("id = ?", rule.ID).Updates(updates)
	}

	// 聚合推送本轮新命中（异步，不阻塞评估；失败不影响主流程）。
	if notifyOn && len(pushLines) > 0 {
		uid := rules[0].UserID
		content := strings.Join(pushLines, "\n")
		go s.notify.Send(uid, "QuantVista 提醒命中", content)
	}
	return hits
}

// StartAlertJobs 后台评估提醒：启动延迟 60s 一次 + 每 15 分钟一次，遍历有 active 规则的用户。
// 只做行情类（财报日历类由 service/finance.go 的每日 job 调 EvaluateEarningsAll）。
func StartAlertJobs(mgr *datasource.Manager) {
	svc := NewAlertService(NewMarketService(mgr))
	run := func() {
		if common.DB == nil {
			return
		}
		var userIDs []int64
		if err := common.DB.Model(&model.AlertRule{}).
			Where("status = ? AND kind NOT IN ?", model.AlertStatusActive, earnAlertKinds).
			Distinct().Pluck("user_id", &userIDs).Error; err != nil {
			common.SysWarn("提醒评估列举用户失败: %v", err)
			return
		}
		for _, uid := range userIDs {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
			if n, err := svc.evaluateUserMarket(ctx, uid); err != nil {
				common.SysWarn("评估用户 %d 提醒失败: %v", uid, err)
			} else if n > 0 {
				common.SysLog("用户 %d 提醒命中 %d 条", uid, n)
			}
			cancel()
		}
	}
	go func() {
		time.Sleep(60 * time.Second)
		run()
		t := time.NewTicker(15 * time.Minute)
		defer t.Stop()
		for range t.C {
			run()
		}
	}()
}

// --- 财报日历类评估（earn_date / earn_fcst，每日一评） ---

// evaluateEarnDate 纯函数：距预约披露日 ≤N 自然日（N=threshold）命中。
// 防重（Once=false 时窗口内每天评估都会满足条件）：本披露窗口内已提醒过
//（TriggeredAt 晚于「披露日-N 天」）则不再命中。返回（命中、观测值=剩余天数、说明）。
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

// evaluateEarnRulesForUser 评估某用户全部 active 财报类规则（查本地表，不打上游）。
// 命中口径与行情类同：recordEvent 落明细（同日去重）、更新规则行、聚合推送。
func (s *AlertService) evaluateEarnRulesForUser(userID int64) int {
	var rules []model.AlertRule
	if err := common.DB.Where("user_id = ? AND status = ? AND kind IN ?",
		userID, model.AlertStatusActive, earnAlertKinds).Find(&rules).Error; err != nil || len(rules) == 0 {
		return 0
	}
	now := time.Now()
	today := now.In(time.Local).Format("2006-01-02")
	notifyOn := false
	if s.notify != nil {
		notifyOn = s.notify.HasEnabledChannel(userID) && userNotifyEnabled(userID)
	}
	var pushLines []string
	hits := 0

	for _, rule := range rules {
		var triggered bool
		var value float64
		var msg string
		switch rule.Kind {
		case model.AlertKindEarnDate:
			if sched := UpcomingDisclosure(rule.Symbol, today); sched != nil {
				triggered, value, msg = evaluateEarnDate(rule, sched.AppointDate, sched.ReportTypeName, now)
			}
		case model.AlertKindEarnFcst:
			triggered, value, msg = evaluateEarnFcst(rule, LatestForecast(rule.Symbol), now)
		}

		updates := map[string]any{"last_value": round2(value), "last_check_date": today}
		if triggered {
			hits++
			ts := time.Now()
			updates["triggered_at"] = &ts
			updates["trigger_msg"] = truncateRunes(msg, 256)
			if rule.Once {
				updates["status"] = model.AlertStatusTriggered
			}
			recordEvent(rule, msg, today, ts)
			if notifyOn && (rule.TriggeredAt == nil || rule.TriggeredAt.In(time.Local).Format("2006-01-02") != today) {
				name := rule.Name
				if name == "" {
					name = rule.Symbol
				}
				pushLines = append(pushLines, name+"("+rule.Symbol+")："+msg)
			}
		}
		common.DB.Model(&model.AlertRule{}).Where("id = ?", rule.ID).Updates(updates)
	}

	if notifyOn && len(pushLines) > 0 {
		content := strings.Join(pushLines, "\n")
		go s.notify.Send(userID, "QuantVista 财报提醒", content)
	}
	return hits
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
