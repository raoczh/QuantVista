package service

import (
	"encoding/json"
	"errors"
	"math"
	"strings"

	"quantvista/common"
	"quantvista/model"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type UserService struct{}

func NewUserService() *UserService { return &UserService{} }

// GetByID 取用户（不含密码）。
func (s *UserService) GetByID(id int64) (*model.User, error) {
	var u model.User
	if err := common.DB.First(&u, id).Error; err != nil {
		return nil, errors.New("用户不存在")
	}
	u.Password = ""
	return &u, nil
}

// GetPreference 取用户偏好，不存在则建默认（流动性门槛默认 1 亿元，与常量 minCandidateAmount 同源）。
func (s *UserService) GetPreference(userID int64) (*model.UserPreference, error) {
	if common.DB == nil || userID <= 0 {
		return nil, errors.New("偏好数据库或用户无效")
	}
	var p model.UserPreference
	if err := common.DB.Where("user_id = ?", userID).First(&p).Error; err == nil {
		return &p, nil
	} else if !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, err
	}
	if err := common.DB.Clauses(clause.OnConflict{DoNothing: true}).Create(&model.UserPreference{
		UserID: userID, MinCandidateAmount: defaultMinCandidateAmount,
	}).Error; err != nil {
		return nil, err
	}
	if err := common.DB.Where("user_id = ?", userID).First(&p).Error; err != nil {
		return nil, err
	}
	return &p, nil
}

// PreferenceInput 只更新明确提交的字段；nil 为保留，false、0、空串仍是有效的显式值。
type PreferenceInput struct {
	RiskLevel       *string `json:"risk_level"`
	DefaultMarket   *string `json:"default_market"`
	HorizonPref     *string `json:"horizon_pref"`
	DefaultRecCount *int    `json:"default_rec_count"`
	EnableNotify    *bool   `json:"enable_notify"`

	BlacklistJSON      *string  `json:"blacklist_json"`       // 候选池黑名单 [{symbol,market,reason}]
	MinCandidateAmount *float64 `json:"min_candidate_amount"` // 候选池最低日成交额（元；0=不过滤）
	RecFiltersJSON     *string  `json:"rec_filters_json"`     // 推荐筛选默认值（RecFilters JSON；空=类型默认）

	EnableDailyReport *bool `json:"enable_daily_report"` // 收盘日报（今日复盘+明日推荐）自动生成

	TotalCapital    *float64 `json:"total_capital"`     // 总投资资金（元；0=未设置，持仓 AI 不注入资金上下文）
	GuardConfigJSON *string  `json:"guard_config_json"` // 智能守护配置（guardConfig JSON；空=默认全开）

	InvestmentGuideVersion *int    `json:"investment_guide_version"`
	InvestmentGuideStatus  *string `json:"investment_guide_status"`
}

// BlacklistEntry 候选池黑名单条目（用户配置的回避规则）。
type BlacklistEntry struct {
	Symbol string `json:"symbol"`
	Market string `json:"market"`
	Reason string `json:"reason"`
}

// defaultMinCandidateAmount 新用户偏好的流动性门槛默认值（元）。
const defaultMinCandidateAmount = 1e8

// maxBlacklistEntries 黑名单条数上限（个人自用，防误传超大 JSON）。
const maxBlacklistEntries = 100

// normalizeBlacklist 解析并归一化黑名单 JSON：去空/去重/市场缺省 cn/理由截断。
// 空串或空数组返回 ""（不落无意义数据）。
func normalizeBlacklist(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}
	var entries []BlacklistEntry
	if err := json.Unmarshal([]byte(raw), &entries); err != nil {
		return "", errors.New("黑名单格式错误：应为 [{symbol,market,reason}] 数组")
	}
	if len(entries) > maxBlacklistEntries {
		return "", errors.New("黑名单最多 100 条")
	}
	out := make([]BlacklistEntry, 0, len(entries))
	seen := map[string]bool{}
	for _, e := range entries {
		sym := strings.TrimSpace(e.Symbol)
		if sym == "" {
			continue
		}
		mkt := strings.ToLower(strings.TrimSpace(e.Market))
		if mkt == "" {
			mkt = "cn"
		}
		key := mkt + ":" + sym
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, BlacklistEntry{Symbol: sym, Market: mkt, Reason: truncateRunes(strings.TrimSpace(e.Reason), 100)})
	}
	if len(out) == 0 {
		return "", nil
	}
	b, err := json.Marshal(out)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

const (
	HorizonShortTerm = "short_term"
	HorizonMidTerm   = "mid_term"
	HorizonLongTerm  = "long_term"

	RecommendationTypeShortTerm = "short_term"
	RecommendationTypeLongTerm  = "long_term"

	// InvestmentGuideCurrentVersion 三问向导题目/语义版本。字段默认值不能代表用户作答；
	// 前端仅在保存完成或明确跳过时写入当前版本。
	InvestmentGuideCurrentVersion = 1
	InvestmentGuideNotStarted     = "not_started"
	InvestmentGuideCompleted      = "completed"
	InvestmentGuideSkipped        = "skipped"
)

var (
	validRisk    = map[string]bool{"conservative": true, "balanced": true, "aggressive": true}
	validMarket  = map[string]bool{"cn": true, "us": true, "hk": true}
	validHorizon = map[string]bool{HorizonShortTerm: true, HorizonMidTerm: true, HorizonLongTerm: true}
)

// RecommendationTypeForHorizon 将用户周期偏好映射到落库推荐类型。
// mid_term 只作为偏好存在，推荐记录仍写入 short_term / long_term。
func RecommendationTypeForHorizon(horizon string) string {
	switch horizon {
	case HorizonShortTerm:
		return RecommendationTypeShortTerm
	case HorizonMidTerm, HorizonLongTerm:
		return RecommendationTypeLongTerm
	default:
		return RecommendationTypeLongTerm
	}
}

// UpdatePreference 锁内合并字段并校验，避免不同页面的保存互相覆盖。
func (s *UserService) UpdatePreference(userID int64, in PreferenceInput) (*model.UserPreference, error) {
	// 默认行在事务外初始化，不在等待锁前建立 MySQL 可重复读快照。
	if _, err := s.GetPreference(userID); err != nil {
		return nil, err
	}
	var p model.UserPreference
	err := common.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("user_id = ?", userID).First(&p).Error; err != nil {
			return err
		}
		priorVersion, priorStatus := p.InvestmentGuideVersion, p.InvestmentGuideStatus
		if err := applyPreferenceInput(&p, in); err != nil {
			return err
		}
		if p.InvestmentGuideVersion < priorVersion {
			return errors.New("投资偏好向导状态已更新，请刷新后重试")
		}
		if p.InvestmentGuideVersion == priorVersion && priorStatus == InvestmentGuideCompleted && p.InvestmentGuideStatus == InvestmentGuideSkipped {
			p.InvestmentGuideStatus = InvestmentGuideCompleted
		}
		if err := validatePreference(&p); err != nil {
			return err
		}
		if err := tx.Model(&model.UserPreference{}).Where("id = ? AND user_id = ?", p.ID, userID).
			Select("*").Omit("id", "user_id", "created_at").Updates(&p).Error; err != nil {
			return err
		}
		// 普通字段的保存不重新推进用户正在进行的引导。
		if in.InvestmentGuideVersion == nil && in.InvestmentGuideStatus == nil {
			return nil
		}
		switch p.InvestmentGuideStatus {
		case InvestmentGuideCompleted:
			return setOnboardingStepTx(tx, userID, OnboardingStepPreference, model.OnboardingStepCompleted, 0)
		case InvestmentGuideSkipped:
			return setOnboardingStepTx(tx, userID, OnboardingStepPreference, model.OnboardingStepSkipped, 0)
		default:
			return nil
		}
	})
	if err != nil {
		return nil, err
	}
	return &p, nil
}

func applyPreferenceInput(p *model.UserPreference, in PreferenceInput) error {
	if in.RiskLevel != nil {
		p.RiskLevel = *in.RiskLevel
	}
	if in.DefaultMarket != nil {
		p.DefaultMarket = *in.DefaultMarket
	}
	if in.HorizonPref != nil {
		p.HorizonPref = *in.HorizonPref
	}
	if in.DefaultRecCount != nil {
		p.DefaultRecCount = *in.DefaultRecCount
	}
	if in.EnableNotify != nil {
		p.EnableNotify = *in.EnableNotify
	}
	if in.MinCandidateAmount != nil {
		p.MinCandidateAmount = *in.MinCandidateAmount
	}
	if in.EnableDailyReport != nil {
		p.EnableDailyReport = *in.EnableDailyReport
	}
	if in.TotalCapital != nil {
		p.TotalCapital = *in.TotalCapital
	}
	if in.InvestmentGuideVersion != nil {
		p.InvestmentGuideVersion = *in.InvestmentGuideVersion
	}
	if in.InvestmentGuideStatus != nil {
		p.InvestmentGuideStatus = strings.TrimSpace(*in.InvestmentGuideStatus)
	}
	for _, field := range []struct {
		input     *string
		dest      *string
		normalize func(string) (string, error)
	}{
		{in.BlacklistJSON, &p.BlacklistJSON, normalizeBlacklist},
		{in.RecFiltersJSON, &p.RecFiltersJSON, normalizeRecFiltersJSON},
		{in.GuardConfigJSON, &p.GuardConfigJSON, normalizeGuardConfigJSON},
	} {
		if field.input == nil {
			continue
		}
		value, err := field.normalize(*field.input)
		if err != nil {
			return err
		}
		*field.dest = value
	}
	return nil
}

func validatePreference(in *model.UserPreference) error {
	if !validRisk[in.RiskLevel] {
		return errors.New("非法的风险等级")
	}
	if !validMarket[in.DefaultMarket] {
		return errors.New("非法的默认市场")
	}
	if !validHorizon[in.HorizonPref] {
		return errors.New("非法的默认周期")
	}
	if in.DefaultRecCount < 3 || in.DefaultRecCount > 5 {
		return errors.New("默认推荐数量需在 3~5 之间")
	}
	if math.IsNaN(in.TotalCapital) || math.IsInf(in.TotalCapital, 0) || in.TotalCapital < 0 || in.TotalCapital > 1e12 {
		return errors.New("总投资资金需在 0~1万亿 之间（0=未设置）")
	}
	guideStatus := strings.TrimSpace(in.InvestmentGuideStatus)
	if guideStatus == "" && in.InvestmentGuideVersion == 0 {
		guideStatus = InvestmentGuideNotStarted
	}
	if in.InvestmentGuideVersion < 0 || in.InvestmentGuideVersion > InvestmentGuideCurrentVersion {
		return errors.New("非法的投资偏好向导版本")
	}
	if in.InvestmentGuideVersion == 0 {
		if guideStatus != InvestmentGuideNotStarted {
			return errors.New("未完成的投资偏好向导状态无效")
		}
	} else if guideStatus != InvestmentGuideCompleted && guideStatus != InvestmentGuideSkipped {
		return errors.New("非法的投资偏好向导状态")
	}
	if guideStatus == InvestmentGuideCompleted && in.TotalCapital <= 0 {
		return errors.New("完成投资偏好向导时需填写大于 0 的总投资资金")
	}
	if math.IsNaN(in.MinCandidateAmount) || math.IsInf(in.MinCandidateAmount, 0) || in.MinCandidateAmount < 0 || in.MinCandidateAmount > 1e12 {
		return errors.New("候选池最低成交额需在 0~1万亿 之间（0=不过滤）")
	}
	in.InvestmentGuideStatus = guideStatus
	return nil
}

// normalizeRecFiltersJSON 校验并归一化推荐筛选默认值 JSON：空串通过（用类型默认）、
// 坏格式报错、合法则 sanitize 后重新序列化（去掉未知字段与越界值）。
func normalizeRecFiltersJSON(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}
	var f RecFilters
	if err := json.Unmarshal([]byte(raw), &f); err != nil {
		return "", errors.New("推荐筛选条件格式错误")
	}
	f = sanitizeRecFilters(f)
	b, err := json.Marshal(f)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// normalizeGuardConfigJSON 校验并归一化智能守护配置 JSON：空串通过（服务层用默认全开）、
// 坏格式报错、合法则 sanitize（阈值钳制）后重新序列化。
func normalizeGuardConfigJSON(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}
	c, err := decodeGuardConfig(raw)
	if err != nil {
		return "", err
	}
	b, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// GetQuota 取用户配额，不存在则建默认。
func (s *UserService) GetQuota(userID int64) (*model.UserQuota, error) {
	return getUserQuota(userID)
}

// ChangePassword 修改密码。已有密码的账号需校验旧密码；纯 OAuth 账号（无密码）允许首次设置。
// 成功后吊销该用户全部刷新令牌，强制其它会话重新登录。
func (s *UserService) ChangePassword(userID int64, oldPw, newPw string) error {
	if len(newPw) < 8 {
		return errors.New("新密码至少 8 个字符")
	}
	if len(newPw) > 72 {
		return errors.New("新密码过长（bcrypt 上限 72 字节）")
	}
	var u model.User
	if err := common.DB.First(&u, userID).Error; err != nil {
		return errors.New("用户不存在")
	}
	if u.Password != "" && !common.CheckPassword(u.Password, oldPw) {
		return errors.New("原密码不正确")
	}
	hash, err := common.HashPassword(newPw)
	if err != nil {
		return err
	}
	// 改密三步须原子且逐步检查错误：写新密码 + token_version+1（旧 access token 即时失效）
	// + 吊销全部刷新令牌（强制所有会话重登）。任一步失败整体回滚，避免「密码已改但旧会话/
	// 旧刷新令牌仍有效」的安全裂缝（原实现后两步在事务外且吞错）。
	return common.DB.Transaction(func(tx *gorm.DB) error {
		current, err := lockEnabledAuthUser(tx, userID)
		if err != nil {
			return err
		}
		if current.TokenVersion != u.TokenVersion || current.Password != u.Password {
			return errors.New("账号凭证已变更，请重新登录后修改密码")
		}
		if err := tx.Model(current).Updates(map[string]any{
			"password": hash, "token_version": gorm.Expr("token_version + 1"),
		}).Error; err != nil {
			return err
		}
		return tx.Model(&model.RefreshToken{}).Where("user_id = ?", userID).Update("revoked", true).Error
	})
}
