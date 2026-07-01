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
	alertBarLimit    = 90 // 评估 ma/breakout 需要的日线条数
	maxAlertsPerUser = 200
)

var validAlertKind = map[string]bool{
	model.AlertKindPrice:     true,
	model.AlertKindPctChange: true,
	model.AlertKindMA:        true,
	model.AlertKindBreakout:  true,
}

// alertEval 评估的输入观测值（纯函数便于测试）。
type alertEval struct {
	Price     float64
	DayHigh   float64
	DayLow    float64
	ChangePct float64
	Closes    []float64 // 升序日线收盘（最新在末尾），供 ma/breakout
	Highs     []float64
	Lows      []float64
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
		Kind: in.Kind, Op: in.Op, Threshold: round2(in.Threshold), Period: in.Period,
		Once: in.Once, Note: strings.TrimSpace(in.Note), Status: model.AlertStatusActive,
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
	rule.Threshold = round2(in.Threshold)
	rule.Period = in.Period
	rule.Once = in.Once
	rule.Note = strings.TrimSpace(in.Note)
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

// Delete 删除规则（仅本人）。
func (s *AlertService) Delete(userID, id int64) error {
	res := common.DB.Where("id = ? AND user_id = ?", id, userID).Delete(&model.AlertRule{})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return errors.New("提醒规则不存在")
	}
	return nil
}

// TriggeredForUser 返回用户当前应进入今日待办的命中规则。
// once 规则命中后置 triggered，作为待处理项持续保留；
// 非 once 规则保持 active，仅当 triggered_at 的日期为今天（今天确有命中）才纳入——
// 不能用 last_check_date 判定，它每次评估都刷成今天，会让历史命中天天误报。
func (s *AlertService) TriggeredForUser(userID int64) ([]model.AlertRule, error) {
	var rows []model.AlertRule
	err := common.DB.Where(
		"user_id = ? AND (status = ? OR triggered_at IS NOT NULL)",
		userID, model.AlertStatusTriggered,
	).
		Order("triggered_at DESC").Find(&rows).Error
	if err != nil {
		return nil, err
	}
	today := time.Now().In(time.Local).Format("2006-01-02")
	out := rows[:0]
	for _, r := range rows {
		if r.Status == model.AlertStatusTriggered {
			out = append(out, r) // once 命中态：作为待处理项保留
			continue
		}
		if r.TriggeredAt != nil && r.TriggeredAt.In(time.Local).Format("2006-01-02") == today {
			out = append(out, r) // 非 once：今天确有命中才纳入
		}
	}
	return out, nil
}

// --- 评估编排 ---

// EvaluateUser 评估某用户全部 active 规则；命中则落库（once 规则命中后置 triggered）。返回命中数。
func (s *AlertService) EvaluateUser(ctx context.Context, userID int64) (int, error) {
	var rules []model.AlertRule
	if err := common.DB.Where("user_id = ? AND status = ?", userID, model.AlertStatusActive).Find(&rules).Error; err != nil {
		return 0, err
	}
	return s.evaluateRules(ctx, rules), nil
}

// evaluateRules 评估一批规则，按 symbol 缓存行情/日线，避免重复请求。
func (s *AlertService) evaluateRules(ctx context.Context, rules []model.AlertRule) int {
	type md struct {
		eval alertEval
		ok   bool
	}
	cache := map[string]md{}
	today := time.Now().In(time.Local).Format("2006-01-02")
	hits := 0

	// 用户是否配置了启用的推送通道（一批规则同属一个用户，只查一次）。
	notifyOn := false
	if s.notify != nil && len(rules) > 0 {
		notifyOn = s.notify.HasEnabledChannel(rules[0].UserID)
	}
	var pushLines []string

	for _, rule := range rules {
		key := QuoteKey(rule.Market, rule.Symbol)
		data, cached := cache[key]
		if !cached {
			data = md{}
			if q, err := s.market.GetQuote(ctx, rule.Market, rule.Symbol); err == nil && q.Price > 0 {
				data.eval = alertEval{Price: q.Price, DayHigh: q.High, DayLow: q.Low, ChangePct: q.ChangePct}
				data.ok = true
				// ma/breakout 才需要日线。
				if rule.Kind == model.AlertKindMA || rule.Kind == model.AlertKindBreakout {
					if bars, berr := s.market.GetDailyBars(ctx, rule.Market, rule.Symbol, alertBarLimit); berr == nil {
						for _, b := range bars {
							data.eval.Closes = append(data.eval.Closes, b.Close)
							data.eval.Highs = append(data.eval.Highs, b.High)
							data.eval.Lows = append(data.eval.Lows, b.Low)
						}
					}
				}
			}
			cache[key] = data
		}
		if !data.ok {
			continue
		}
		// 若已缓存但缺日线而本规则需要，补拉一次。
		if (rule.Kind == model.AlertKindMA || rule.Kind == model.AlertKindBreakout) && len(data.eval.Closes) == 0 {
			if bars, berr := s.market.GetDailyBars(ctx, rule.Market, rule.Symbol, alertBarLimit); berr == nil {
				for _, b := range bars {
					data.eval.Closes = append(data.eval.Closes, b.Close)
					data.eval.Highs = append(data.eval.Highs, b.High)
					data.eval.Lows = append(data.eval.Lows, b.Low)
				}
				cache[key] = data
			}
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
func StartAlertJobs(mgr *datasource.Manager) {
	svc := NewAlertService(NewMarketService(mgr))
	run := func() {
		if common.DB == nil {
			return
		}
		var userIDs []int64
		if err := common.DB.Model(&model.AlertRule{}).
			Where("status = ?", model.AlertStatusActive).
			Distinct().Pluck("user_id", &userIDs).Error; err != nil {
			common.SysWarn("提醒评估列举用户失败: %v", err)
			return
		}
		for _, uid := range userIDs {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
			if n, err := svc.EvaluateUser(ctx, uid); err != nil {
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
