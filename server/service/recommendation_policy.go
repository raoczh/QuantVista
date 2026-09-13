package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"quantvista/model"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const recommendationAdditiveVersion = "additive_sp1"

type rankingArtifactPayload struct {
	Version string                 `json:"version"`
	Report  *RankingResearchReport `json:"report"`
	Model   *RankingRidgeModel     `json:"model"`
}

type recScoringRuntime struct {
	Algorithm    string             `json:"algorithm"`
	ArtifactID   int64              `json:"artifact_id,omitempty"`
	ArtifactHash string             `json:"artifact_hash,omitempty"`
	Model        *RankingRidgeModel `json:"model,omitempty"`
}

func validRankingAlgorithm(v string) bool {
	return v == recommendationScoringVersion || v == recommendationAdditiveVersion || v == rankingRidgeVersion
}

func validateRankingRidge(m *RankingRidgeModel) error {
	p := len(rankingFeatureNames)
	if m == nil || m.Version != rankingRidgeVersion || m.Samples < 100 || m.Dates < 20 || !finiteRecNumber(m.Intercept) || !finiteRecNumber(m.Lambda) || m.Lambda <= 0 || len(m.Means) != p || len(m.Scales) != p || len(m.MissingRates) != p || len(m.Active) != p || len(m.DesignMeans) != 2*p || len(m.Weights) != 2*p {
		return errors.New("学习模型版本、维度或训练覆盖无效")
	}
	for i := 0; i < p; i++ {
		if !finiteRecNumber(m.Means[i]) || !finiteRecNumber(m.Scales[i]) || m.Scales[i] <= 0 || !finiteRecNumber(m.MissingRates[i]) || m.MissingRates[i] < 0 || m.MissingRates[i] > 1 {
			return errors.New("学习模型预处理参数无效")
		}
	}
	for i, w := range m.Weights {
		name := rankingFeatureNames[i%p]
		if i >= p {
			name = "missing:" + name
		}
		if w.Feature != name || !finiteRecNumber(w.Weight) || !finiteRecNumber(m.DesignMeans[i]) {
			return errors.New("学习模型特征顺序或系数无效")
		}
	}
	return nil
}

func validateRecScoringRuntime(r recScoringRuntime) error {
	if !validRankingAlgorithm(r.Algorithm) {
		return errors.New("不支持该评分算法版本，请重新选择")
	}
	if r.Algorithm == rankingRidgeVersion {
		if r.ArtifactID <= 0 || len(r.ArtifactHash) != 64 {
			return errors.New("学习评分缺少不可变模型引用")
		}
		return validateRankingRidge(r.Model)
	}
	if r.Model != nil || r.ArtifactID != 0 || r.ArtifactHash != "" {
		return errors.New("规则评分不能混入学习模型")
	}
	return nil
}

func rankingJSONHash(b []byte) string { sum := sha256.Sum256(b); return hex.EncodeToString(sum[:]) }

func readRankingArtifact(row model.RankingModelArtifact) (*rankingArtifactPayload, error) {
	if row.Version != rankingRidgeVersion || row.ArtifactHash != rankingJSONHash([]byte(row.Payload)) {
		return nil, errors.New("模型工件版本或摘要不一致")
	}
	var p rankingArtifactPayload
	if err := json.Unmarshal([]byte(row.Payload), &p); err != nil {
		return nil, errors.New("模型工件内容无法解析")
	}
	if p.Version != "ra1" || p.Report == nil || p.Report.Version != rankingResearchVersion || p.Report.Request.RecType != row.RecType || p.Report.Request.Profile != row.Profile || p.Report.Request.Horizon != row.Horizon || p.Report.Request.Target != row.Target || p.Report.Request.AsOf != row.AsOf || p.Report.DatasetHash != row.DatasetHash || p.Report.PromotionReady != row.Eligible {
		return nil, errors.New("模型工件与评估依据不一致")
	}
	if err := validateRankingRidge(p.Model); err != nil {
		return nil, err
	}
	return &p, nil
}

// CaptureRankingArtifact 重新核对用户正在看的机会集摘要后保存模型；不修改评分政策。
// 不接受客户端传来的系数、通过标记或收益结论。
func CaptureRankingArtifact(ctx context.Context, db *gorm.DB, actorID int64, req RankingResearchRequest, expectedHash string) (*model.RankingModelArtifact, error) {
	if len(expectedHash) != 64 {
		return nil, errors.New("请先运行研究并核对数据摘要")
	}
	rep, err := RunRankingResearch(ctx, db, req)
	if err != nil {
		return nil, err
	}
	if rep.DatasetHash != expectedHash {
		return nil, errors.New("样本已变化，请重新查看研究结果后保存模型")
	}
	var selected *RankingRidgeModel
	if len(rep.Folds) > 0 {
		selected = rep.Folds[len(rep.Folds)-1].Model
	}
	if selected == nil {
		return nil, errors.New("最近时间窗口尚无可验证模型，继续积累成熟样本")
	}
	if err := validateRankingRidge(selected); err != nil {
		return nil, err
	}
	// 生成时刻不参与同一工件的身份，避免重复点击保存相同模型。
	rep.GeneratedAt = time.Time{}
	payload, err := json.Marshal(rankingArtifactPayload{Version: "ra1", Report: rep, Model: selected})
	if err != nil {
		return nil, err
	}
	r := rep.Request
	row := &model.RankingModelArtifact{RecType: r.RecType, Profile: r.Profile, Horizon: r.Horizon, Target: r.Target, Version: rankingRidgeVersion, DatasetHash: rep.DatasetHash, ArtifactHash: rankingJSONHash(payload), Eligible: rep.PromotionReady, AsOf: r.AsOf, Payload: string(payload), CreatedBy: actorID}
	if err := db.WithContext(ctx).Clauses(clause.OnConflict{DoNothing: true}).Create(row).Error; err != nil {
		return nil, err
	}
	if err := db.WithContext(ctx).Where("artifact_hash = ?", row.ArtifactHash).First(row).Error; err != nil {
		return nil, err
	}
	return row, nil
}

func loadRecScoringRuntime(ctx context.Context, db *gorm.DB, recType, profile string) (recScoringRuntime, error) {
	r := recScoringRuntime{Algorithm: recommendationScoringVersion}
	if db == nil {
		return r, errors.New("评分配置数据库不可用")
	}
	var policy model.RankingScoringPolicy
	err := db.WithContext(ctx).Where("`key` = ?", recType+":"+profile).First(&policy).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return r, nil
	}
	if err != nil {
		return r, err
	}
	if policy.RecType != recType || policy.Profile != profile {
		return r, errors.New("评分政策与策略配置不一致")
	}
	r.Algorithm = policy.Algorithm
	// 旧规则选择随发布升级为当前规则，批次仍冻结实际版本；不改写已有政策行
	// 或历史输出。学习模型必须重新通过当前特征版本的检验，不能自动迁移权重。
	if r.Algorithm == "qr1" {
		r.Algorithm = recommendationScoringVersion
	}
	if r.Algorithm == rankingRidgeVersion {
		var row model.RankingModelArtifact
		if err := db.WithContext(ctx).Where("id = ?", policy.ArtifactID).First(&row).Error; err != nil {
			return r, err
		}
		p, err := readRankingArtifact(row)
		if err != nil {
			return r, err
		}
		if !row.Eligible || row.RecType != recType || row.Profile != profile {
			return r, errors.New("学习模型未通过启用条件或不适用当前策略")
		}
		r.ArtifactID, r.ArtifactHash, r.Model = row.ID, row.ArtifactHash, p.Model
	}
	return r, validateRecScoringRuntime(r)
}

type RankingPolicyUpdate struct {
	RecType      string `json:"rec_type"`
	Profile      string `json:"profile"`
	Algorithm    string `json:"algorithm"`
	ArtifactID   int64  `json:"artifact_id"`
	BaseRevision int64  `json:"base_revision"`
}

func UpdateRankingPolicy(ctx context.Context, db *gorm.DB, actorID int64, req RankingPolicyUpdate) (*model.RankingScoringPolicy, error) {
	if (req.RecType != model.RecTypeShortTerm && req.RecType != model.RecTypeLongTerm) || req.Profile == "" || !model.ValidStrategyScoreProfile(req.Profile) || !validRankingAlgorithm(req.Algorithm) || req.BaseRevision < 0 {
		return nil, errors.New("评分政策参数无效")
	}
	if req.Algorithm != rankingRidgeVersion && req.ArtifactID != 0 {
		return nil, errors.New("规则评分不应指定模型工件")
	}
	key := req.RecType + ":" + req.Profile
	row := &model.RankingScoringPolicy{Key: key, RecType: req.RecType, Profile: req.Profile, Algorithm: req.Algorithm, ArtifactID: req.ArtifactID, Revision: req.BaseRevision + 1, UpdatedBy: actorID, UpdatedAt: time.Now()}
	err := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if req.Algorithm == rankingRidgeVersion {
			var artifact model.RankingModelArtifact
			if err := tx.Where("id = ?", req.ArtifactID).First(&artifact).Error; err != nil {
				return errors.New("指定的学习模型不存在")
			}
			payload, err := readRankingArtifact(artifact)
			if err != nil {
				return err
			}
			if !artifact.Eligible || !payload.Report.Evaluated || len(payload.Report.PromotionReasons) > 0 || payload.Report.Request.Source != "recommendations" || artifact.RecType != req.RecType || artifact.Profile != req.Profile {
				return errors.New("该模型尚未通过真实机会集的时间外检验，不能启用")
			}
		}
		before := model.RankingScoringPolicy{Key: key, RecType: req.RecType, Profile: req.Profile, Algorithm: recommendationScoringVersion}
		err := tx.Where("`key` = ?", key).First(&before).Error
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		if before.Revision != req.BaseRevision {
			return errors.New("评分政策已被其他页面修改，请刷新后重试")
		}
		var result *gorm.DB
		if errors.Is(err, gorm.ErrRecordNotFound) {
			result = tx.Clauses(clause.OnConflict{DoNothing: true}).Create(row)
		} else {
			result = tx.Model(&model.RankingScoringPolicy{}).Where("`key` = ? AND revision = ?", key, req.BaseRevision).Updates(map[string]any{"algorithm": row.Algorithm, "artifact_id": row.ArtifactID, "revision": row.Revision, "updated_by": row.UpdatedBy, "updated_at": row.UpdatedAt})
		}
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return errors.New("评分政策已变化，请刷新后重试")
		}
		b, _ := json.Marshal(before)
		a, _ := json.Marshal(row)
		return tx.Create(&model.RankingPolicyAudit{PolicyKey: key, BeforeJSON: string(b), AfterJSON: string(a), ActorID: actorID}).Error
	})
	return row, err
}

type RankingPolicyState struct {
	DefaultAlgorithm string                       `json:"default_algorithm"`
	Policies         []model.RankingScoringPolicy `json:"policies"`
	Artifacts        []model.RankingModelArtifact `json:"artifacts"`
	Changes          []model.RankingPolicyAudit   `json:"changes"`
}

func ReadRankingPolicyState(ctx context.Context, db *gorm.DB) (*RankingPolicyState, error) {
	out := &RankingPolicyState{DefaultAlgorithm: recommendationScoringVersion, Policies: []model.RankingScoringPolicy{}, Artifacts: []model.RankingModelArtifact{}, Changes: []model.RankingPolicyAudit{}}
	if err := db.WithContext(ctx).Order("`key`").Find(&out.Policies).Error; err != nil {
		return nil, err
	}
	if err := db.WithContext(ctx).Omit("payload").Order("id DESC").Limit(50).Find(&out.Artifacts).Error; err != nil {
		return nil, err
	}
	if err := db.WithContext(ctx).Order("id DESC").Limit(20).Find(&out.Changes).Error; err != nil {
		return nil, err
	}
	return out, nil
}

func (s *strategyTemplate) scoringAlgorithm() string {
	if s != nil && s.scoring.Algorithm != "" {
		return s.scoring.Algorithm
	}
	return recommendationScoringVersion
}

func learnedCandidateRanking(c candidate, sc ScoreResult, m *RankingRidgeModel) float64 {
	c.ScoreDims = &scoreDims{Trend: sc.Trend, Momentum: sc.Momentum, Position: sc.Position, Volume: sc.Volume, Risk: sc.Risk}
	// 固定平移只为与既有分数展示兼容，不改变线性排序，也不作概率或收益承诺。
	return 50 + m.score(rankingCandidateFeatures(c))
}

func scoringAlgorithmNote(s *strategyTemplate) string {
	if s.scoringAlgorithm() == rankingRidgeVersion {
		return fmt.Sprintf("学习排序使用冻结模型 #%d；分值是排序单位，不是获利概率或预期收益", s.scoring.ArtifactID)
	}
	if s.scoringAlgorithm() == recommendationAdditiveVersion {
		return "本批使用原加法评分对照；当前策略条件、行情检查和执行约束继续生效"
	}
	return "本批按技术、形态、入场、风险、财务与背景分组排序；分值不是获利概率"
}
