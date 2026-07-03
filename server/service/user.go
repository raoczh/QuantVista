package service

import (
	"encoding/json"
	"errors"
	"strings"

	"quantvista/common"
	"quantvista/model"

	"gorm.io/gorm"
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
	var p model.UserPreference
	if err := common.DB.Where(model.UserPreference{UserID: userID}).
		Attrs(model.UserPreference{MinCandidateAmount: defaultMinCandidateAmount}).
		FirstOrCreate(&p).Error; err != nil {
		return nil, err
	}
	return &p, nil
}

// PreferenceInput 偏好更新入参。
type PreferenceInput struct {
	RiskLevel       string `json:"risk_level"`
	DefaultMarket   string `json:"default_market"`
	HorizonPref     string `json:"horizon_pref"`
	DefaultRecCount int    `json:"default_rec_count"`
	EnableNotify    bool   `json:"enable_notify"`

	BlacklistJSON      string  `json:"blacklist_json"`       // 候选池黑名单 [{symbol,market,reason}]
	MinCandidateAmount float64 `json:"min_candidate_amount"` // 候选池最低日成交额（元；0=不过滤）

	EnableDailyReport bool `json:"enable_daily_report"` // 收盘日报（今日复盘+明日推荐）自动生成
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

// UpdatePreference 校验并更新用户偏好。
func (s *UserService) UpdatePreference(userID int64, in PreferenceInput) (*model.UserPreference, error) {
	if !validRisk[in.RiskLevel] {
		return nil, errors.New("非法的风险等级")
	}
	if !validMarket[in.DefaultMarket] {
		return nil, errors.New("非法的默认市场")
	}
	if !validHorizon[in.HorizonPref] {
		return nil, errors.New("非法的默认周期")
	}
	if in.DefaultRecCount < 3 || in.DefaultRecCount > 5 {
		return nil, errors.New("默认推荐数量需在 3~5 之间")
	}
	if in.MinCandidateAmount < 0 || in.MinCandidateAmount > 1e12 {
		return nil, errors.New("候选池最低成交额需在 0~1万亿 之间（0=不过滤）")
	}
	blacklist, err := normalizeBlacklist(in.BlacklistJSON)
	if err != nil {
		return nil, err
	}
	p, err := s.GetPreference(userID)
	if err != nil {
		return nil, err
	}
	p.RiskLevel = in.RiskLevel
	p.DefaultMarket = in.DefaultMarket
	p.HorizonPref = in.HorizonPref
	p.DefaultRecCount = in.DefaultRecCount
	p.EnableNotify = in.EnableNotify
	p.BlacklistJSON = blacklist
	p.MinCandidateAmount = in.MinCandidateAmount
	p.EnableDailyReport = in.EnableDailyReport
	if err := common.DB.Save(p).Error; err != nil {
		return nil, err
	}
	return p, nil
}

// GetQuota 取用户配额，不存在则建默认。
func (s *UserService) GetQuota(userID int64) (*model.UserQuota, error) {
	var q model.UserQuota
	if err := common.DB.FirstOrCreate(&q, model.UserQuota{UserID: userID}).Error; err != nil {
		return nil, err
	}
	return &q, nil
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
	if err := common.DB.Model(&u).Update("password", hash).Error; err != nil {
		return err
	}
	// 改密后使旧 access token 即时失效（令牌版本 +1），并吊销该用户全部刷新令牌，强制所有会话重登。
	common.DB.Model(&u).UpdateColumn("token_version", gorm.Expr("token_version + 1"))
	common.DB.Model(&model.RefreshToken{}).Where("user_id = ?", userID).Update("revoked", true)
	return nil
}
