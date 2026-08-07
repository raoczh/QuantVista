package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"quantvista/common"
	"quantvista/model"

	"gorm.io/gorm"
)

const (
	strategyRunRequestMaxBytes = 16 << 10
	strategyRunResultMaxBytes  = 2 << 20
	strategyScanJobTimeout     = 5 * time.Minute
	strategyBacktestJobTimeout = 15 * time.Minute
)

type strategyJobSnapshot struct {
	Version            int    `json:"version"`
	StrategyIdentity   string `json:"strategy_identity"`
	StrategyKey        string `json:"strategy_key,omitempty"`
	StrategyID         int64  `json:"strategy_id,omitempty"`
	StrategyRevisionID int64  `json:"strategy_revision_id,omitempty"`
	StrategyRevision   int    `json:"strategy_revision"`
	StrategyHash       string `json:"strategy_hash"`
	RequestHash        string `json:"request_hash"`
}

type strategyRunSeed struct {
	Kind               string
	StrategyIdentity   string
	StrategyKey        string
	StrategyID         *int64
	StrategyRevisionID *int64
	StrategyRevision   int
	StrategyHash       string
	StrategyName       string
	RequestJSON        string
	RequestHash        string
}

// StrategyRunView 是结果事实的有界 API 投影。列表不填 Request/Result；详情仅在本人
// 查询时返回规范化请求，并且只为成功终态返回结果正文。
type StrategyRunView struct {
	ID                 int64           `json:"id"`
	JobRunID           int64           `json:"job_run_id"`
	Kind               string          `json:"kind"`
	StrategyIdentity   string          `json:"strategy_identity"`
	StrategyKey        string          `json:"strategy_key,omitempty"`
	StrategyID         *int64          `json:"strategy_id,omitempty"`
	StrategyRevisionID *int64          `json:"strategy_revision_id,omitempty"`
	StrategyRevision   int             `json:"strategy_revision"`
	StrategyHash       string          `json:"strategy_hash"`
	StrategyName       string          `json:"strategy_name"`
	RequestHash        string          `json:"request_hash"`
	AsOf               *time.Time      `json:"as_of,omitempty"`
	ContentHash        string          `json:"content_hash,omitempty"`
	Status             string          `json:"status"`
	Error              string          `json:"error,omitempty"`
	ErrorCode          string          `json:"error_code,omitempty"`
	Request            json.RawMessage `json:"request,omitempty"`
	Result             json.RawMessage `json:"result,omitempty"`
	StartedAt          *time.Time      `json:"started_at,omitempty"`
	FinishedAt         *time.Time      `json:"finished_at,omitempty"`
	CreatedAt          time.Time       `json:"created_at"`
	UpdatedAt          time.Time       `json:"updated_at"`
}

func RegisterStrategyResearchJobHandlers(screener *ScreenerService, backtest *BacktestService) {
	if screener == nil || backtest == nil {
		return
	}
	binding := strategyRunBinding()
	registerDurableBusinessJobHandler(JobKindScreenerScan, strategyScanJobTimeout, binding,
		func(ctx context.Context, userID int64, _ bool, raw json.RawMessage) (DurableJobResult, error) {
			row, err := loadStrategyRunForExecution(ctx, userID, JobKindScreenerScan, raw)
			if err != nil {
				return DurableJobResult{}, err
			}
			req, err := frozenScanRequest(row.RequestJSON)
			if err != nil {
				return DurableJobResult{}, err
			}
			result, err := screener.Scan(ctx, userID, req)
			if err != nil {
				return DurableJobResult{}, err
			}
			applyStrategyRunMetadata(row, result, nil)
			return DurableJobResult{Value: result, Status: model.JobStatusSuccess,
				Total: result.Universe, Succeeded: result.Matched}, nil
		})
	registerDurableBusinessJobHandler(JobKindStrategyBacktest, strategyBacktestJobTimeout, binding,
		func(ctx context.Context, userID int64, _ bool, raw json.RawMessage) (DurableJobResult, error) {
			row, err := loadStrategyRunForExecution(ctx, userID, JobKindStrategyBacktest, raw)
			if err != nil {
				return DurableJobResult{}, err
			}
			req, err := frozenBacktestRequest(row.RequestJSON)
			if err != nil {
				return DurableJobResult{}, err
			}
			result, err := backtest.Run(ctx, userID, req)
			if err != nil {
				return DurableJobResult{}, err
			}
			applyStrategyRunMetadata(row, nil, result)
			total, succeeded := 0, 0
			for _, stat := range result.Stats {
				total += stat.Trades + stat.SkipLimitUp + stat.SkipCash + stat.SkipSuspend + stat.Pending + stat.Forced
				succeeded += stat.Trades
			}
			return DurableJobResult{Value: result, Status: model.JobStatusSuccess,
				Total: total, Succeeded: succeeded, Failed: total - succeeded}, nil
		})
}

// frozenScanRequest/frozenBacktestRequest 只从业务结果事实读取已经解析的条件树。
// key/id/revision 仍保留在规范化请求中供审计和界面恢复，但执行时不能再次解析当前
// 内置定义或策略指针，否则服务升级或 A→B 编辑会改变旧作业语义。
func frozenScanRequest(requestJSON string) (ScanRequest, error) {
	var req ScanRequest
	if err := json.Unmarshal([]byte(requestJSON), &req); err != nil {
		return ScanRequest{}, fmt.Errorf("扫描规范化请求无效: %w", err)
	}
	if req.Tree == nil {
		return ScanRequest{}, errors.New("扫描规范化请求缺少策略快照")
	}
	req.StrategyKey, req.StrategyID, req.StrategyRevisionID = "", 0, 0
	return req, nil
}

func frozenBacktestRequest(requestJSON string) (BacktestRequest, error) {
	var req BacktestRequest
	if err := json.Unmarshal([]byte(requestJSON), &req); err != nil {
		return BacktestRequest{}, fmt.Errorf("回测规范化请求无效: %w", err)
	}
	if req.Tree == nil {
		return BacktestRequest{}, errors.New("回测规范化请求缺少策略快照")
	}
	req.StrategyKey, req.StrategyID, req.StrategyRevisionID = "", 0, 0
	return req, nil
}

func applyStrategyRunMetadata(row *model.StrategyRunResult, scan *ScanResult, backtest *BacktestResult) {
	strategyID, revisionID := pointerValue(row.StrategyID), pointerValue(row.StrategyRevisionID)
	if scan != nil {
		scan.Strategy, scan.StrategyID, scan.StrategyRevisionID = row.StrategyName, strategyID, revisionID
		scan.StrategyRevision, scan.StrategyHash = row.StrategyRevision, row.StrategyHash
	}
	if backtest != nil {
		backtest.Strategy, backtest.StrategyID, backtest.StrategyRevisionID = row.StrategyName, strategyID, revisionID
		backtest.StrategyRevision, backtest.StrategyHash = row.StrategyRevision, row.StrategyHash
	}
}

func strategyRunBinding() durableJobBinding {
	return durableJobBinding{
		resultType: JobResultStrategyRun,
		create: func(tx *gorm.DB, run *model.JobRun, _ json.RawMessage) (int64, error) {
			if run.ParentID == nil {
				return 0, errors.New("策略研究作业缺少已规范化的结果种子")
			}
			var parent model.JobRun
			if err := tx.Where("id = ? AND owner_type = ? AND user_id = ? AND result_type = ?",
				*run.ParentID, model.JobOwnerUser, run.UserID, JobResultStrategyRun).First(&parent).Error; err != nil {
				return 0, errors.New("父作业结果引用不可用")
			}
			if parent.ResultID == nil || parent.Kind != run.Kind {
				return 0, errors.New("父作业策略结果类型不匹配")
			}
			var old model.StrategyRunResult
			if err := tx.Where("id = ? AND user_id = ? AND job_run_id = ?", *parent.ResultID, run.UserID, parent.ID).
				First(&old).Error; err != nil {
				return 0, errors.New("父作业策略结果不存在")
			}
			row := model.StrategyRunResult{
				UserID: old.UserID, Kind: old.Kind, StrategyIdentity: old.StrategyIdentity,
				StrategyKey: old.StrategyKey, StrategyID: old.StrategyID,
				StrategyRevisionID: old.StrategyRevisionID, StrategyRevision: old.StrategyRevision,
				StrategyHash: old.StrategyHash, StrategyName: old.StrategyName,
				RequestJSON: old.RequestJSON, RequestHash: old.RequestHash,
				Status: model.JobStatusQueued, JobRunID: run.ID,
			}
			if err := tx.Create(&row).Error; err != nil {
				return 0, err
			}
			return row.ID, nil
		},
		persistSuccess: persistStrategyRunSuccess,
		finishFailure:  finishStrategyRunFailure,
	}
}

func strategyRunSeedBinding(seed strategyRunSeed) durableJobBinding {
	binding := strategyRunBinding()
	binding.create = func(tx *gorm.DB, run *model.JobRun, _ json.RawMessage) (int64, error) {
		row := model.StrategyRunResult{
			UserID: run.UserID, Kind: seed.Kind, StrategyIdentity: seed.StrategyIdentity,
			StrategyKey: seed.StrategyKey, StrategyID: seed.StrategyID,
			StrategyRevisionID: seed.StrategyRevisionID, StrategyRevision: seed.StrategyRevision,
			StrategyHash: seed.StrategyHash, StrategyName: seed.StrategyName,
			RequestJSON: seed.RequestJSON, RequestHash: seed.RequestHash,
			Status: model.JobStatusQueued, JobRunID: run.ID,
		}
		if err := tx.Create(&row).Error; err != nil {
			return 0, err
		}
		return row.ID, nil
	}
	return binding
}

func loadStrategyRunForExecution(ctx context.Context, userID int64, kind string, raw json.RawMessage) (*model.StrategyRunResult, error) {
	if err := ensureEnabledJobUser(userID); err != nil {
		return nil, err
	}
	resultID, ok := currentJobResultID(ctx)
	if !ok {
		return nil, errors.New("策略研究作业缺少结果定位")
	}
	var snapshot strategyJobSnapshot
	if err := json.Unmarshal(raw, &snapshot); err != nil || snapshot.Version != 1 {
		return nil, errors.New("策略研究作业快照无效")
	}
	var row model.StrategyRunResult
	if err := common.DB.Where("id = ? AND user_id = ? AND kind = ?", resultID, userID, kind).First(&row).Error; err != nil {
		return nil, err
	}
	if len(row.RequestJSON) == 0 || len(row.RequestJSON) > strategyRunRequestMaxBytes ||
		sha256Hex([]byte(row.RequestJSON)) != row.RequestHash {
		return nil, errors.New("策略研究规范化请求完整性校验失败")
	}
	if row.RequestHash != snapshot.RequestHash || row.StrategyHash != snapshot.StrategyHash ||
		row.StrategyIdentity != snapshot.StrategyIdentity || row.StrategyKey != snapshot.StrategyKey ||
		pointerValue(row.StrategyID) != snapshot.StrategyID || pointerValue(row.StrategyRevisionID) != snapshot.StrategyRevisionID ||
		row.StrategyRevision != snapshot.StrategyRevision {
		return nil, errors.New("策略研究作业快照与结果引用不一致")
	}
	now := time.Now()
	res := common.DB.Model(&model.StrategyRunResult{}).
		Where("id = ? AND user_id = ? AND job_run_id = ? AND status = ?", row.ID, userID, row.JobRunID, model.JobStatusQueued).
		Updates(map[string]any{"status": model.JobStatusRunning, "started_at": now, "updated_at": now})
	if res.Error != nil {
		return nil, res.Error
	}
	if res.RowsAffected != 1 {
		return nil, errors.New("策略研究结果已被其他执行器接管")
	}
	row.Status, row.StartedAt = model.JobStatusRunning, &now
	return &row, nil
}

func persistStrategyRunSuccess(tx *gorm.DB, run *model.JobRun, _ DurableJobResult, resultJSON []byte, now time.Time) error {
	if run.ResultID == nil {
		return errors.New("策略研究作业缺少结果引用")
	}
	if len(resultJSON) == 0 || len(resultJSON) > strategyRunResultMaxBytes {
		return fmt.Errorf("策略研究结果正文超过 %d 字节上限", strategyRunResultMaxBytes)
	}
	var meta struct {
		TradeDate string `json:"trade_date"`
	}
	if err := json.Unmarshal(resultJSON, &meta); err != nil || strings.TrimSpace(meta.TradeDate) == "" {
		return errors.New("策略研究结果缺少数据 as_of")
	}
	asOf, err := time.ParseInLocation("2006-01-02", meta.TradeDate, time.Local)
	if err != nil {
		return errors.New("策略研究结果数据 as_of 无效")
	}
	contentHash := sha256Hex(resultJSON)
	res := tx.Model(&model.StrategyRunResult{}).
		Where("id = ? AND user_id = ? AND job_run_id = ? AND status = ?", *run.ResultID, run.UserID, run.ID, model.JobStatusRunning).
		Updates(map[string]any{"status": model.JobStatusSuccess, "result_json": string(resultJSON),
			"content_hash": contentHash, "as_of": asOf, "error": "", "error_code": "",
			"finished_at": now, "updated_at": now})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected != 1 {
		return errors.New("策略研究结果终态写入冲突")
	}
	var row model.StrategyRunResult
	if err := tx.Where("id = ? AND user_id = ?", *run.ResultID, run.UserID).First(&row).Error; err != nil {
		return err
	}
	return persistStrategyRunArtifact(tx, run, row, now)
}

func finishStrategyRunFailure(tx *gorm.DB, run *model.JobRun, status, code, message string, now time.Time) error {
	if run.ResultID == nil {
		return errors.New("策略研究作业缺少结果引用")
	}
	res := tx.Model(&model.StrategyRunResult{}).
		Where("id = ? AND user_id = ? AND job_run_id = ? AND status IN ?", *run.ResultID, run.UserID, run.ID,
			[]string{model.JobStatusQueued, model.JobStatusRunning}).
		Updates(map[string]any{"status": status, "error": sanitizeJobError(message), "error_code": code,
			"finished_at": now, "updated_at": now})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		var count int64
		if err := tx.Model(&model.StrategyRunResult{}).
			Where("id = ? AND user_id = ? AND job_run_id = ?", *run.ResultID, run.UserID, run.ID).Count(&count).Error; err != nil {
			return err
		}
		if count != 1 {
			return errors.New("策略研究结果引用无效")
		}
	}
	return nil
}

func (s *ScreenerService) StartScanJob(userID int64, req ScanRequest) (*StrategyRunView, error) {
	seed, snapshot, err := s.prepareScanJob(userID, req)
	if err != nil {
		return nil, err
	}
	return startStrategyRunJob(userID, seed, snapshot)
}

func (s *ScreenerService) prepareScanJob(userID int64, req ScanRequest) (strategyRunSeed, strategyJobSnapshot, error) {
	resolved, err := s.resolveStrategy(userID, req)
	if err != nil {
		return strategyRunSeed{}, strategyJobSnapshot{}, err
	}
	if _, err := validateCondTree(resolved.Tree, 1); err != nil {
		return strategyRunSeed{}, strategyJobSnapshot{}, err
	}
	limit := req.Limit
	if limit <= 0 {
		limit = scanDefaultLimit
	} else if limit > scanMaxLimit {
		limit = scanMaxLimit
	}
	normalized := ScanRequest{IncludeST: req.IncludeST, IncludeStale: req.IncludeStale, Limit: limit}
	seed := strategyRunSeed{Kind: JobKindScreenerScan, StrategyRevision: resolved.Revision,
		StrategyHash: resolved.Hash, StrategyName: resolved.Name}
	switch {
	case req.StrategyKey != "":
		seed.StrategyIdentity, seed.StrategyKey = model.StrategyIdentityBuiltin, req.StrategyKey
		normalized.StrategyKey = req.StrategyKey
	case req.StrategyID > 0:
		strategyID, revisionID := req.StrategyID, resolved.RevisionID
		seed.StrategyIdentity, seed.StrategyID, seed.StrategyRevisionID = model.StrategyIdentityCustom, &strategyID, &revisionID
		normalized.StrategyID, normalized.StrategyRevisionID = strategyID, revisionID
	default:
		seed.StrategyIdentity, seed.StrategyKey = model.StrategyIdentityAdhoc, "adhoc:"+resolved.Hash
	}
	// 业务结果事实必须包含实际执行树。标识字段用于审计/界面恢复，worker 不再通过
	// 它们查询当前代码定义或当前 revision 指针。
	normalized.Tree = resolved.Tree
	return finalizeStrategyRunSeed(seed, normalized)
}

func (s *BacktestService) StartBacktestJob(userID int64, req BacktestRequest) (*StrategyRunView, error) {
	seed, snapshot, err := s.prepareBacktestJob(userID, req)
	if err != nil {
		return nil, err
	}
	return startStrategyRunJob(userID, seed, snapshot)
}

func (s *BacktestService) prepareBacktestJob(userID int64, req BacktestRequest) (strategyRunSeed, strategyJobSnapshot, error) {
	screener := &ScreenerService{}
	resolved, err := screener.resolveStrategy(userID, ScanRequest{StrategyKey: req.StrategyKey,
		StrategyID: req.StrategyID, StrategyRevisionID: req.StrategyRevisionID, Tree: req.Tree})
	if err != nil {
		return strategyRunSeed{}, strategyJobSnapshot{}, err
	}
	if _, err := validateCondTree(resolved.Tree, 1); err != nil {
		return strategyRunSeed{}, strategyJobSnapshot{}, err
	}
	holds, err := normalizeHoldDays(req.HoldDays)
	if err != nil {
		return strategyRunSeed{}, strategyJobSnapshot{}, err
	}
	perCap := req.PerStockCap
	if perCap <= 0 {
		perCap = btDefaultPerCap
	}
	perCap = math.Min(math.Max(perCap, btMinPerCap), btMaxPerCap)
	normalized := BacktestRequest{
		LookbackDays: clampInt(req.LookbackDays, btMinLookback, btMaxLookback, btDefaultLookback),
		SignalCount:  clampInt(req.SignalCount, 1, btMaxSignals, btDefaultSignals),
		HoldDays:     holds, PerStockCap: perCap,
		TopPerDay: clampInt(req.TopPerDay, 10, btDefaultTopPerDay, btDefaultTopPerDay), IncludeST: req.IncludeST,
	}
	seed := strategyRunSeed{Kind: JobKindStrategyBacktest, StrategyRevision: resolved.Revision,
		StrategyHash: resolved.Hash, StrategyName: resolved.Name}
	switch {
	case req.StrategyKey != "":
		seed.StrategyIdentity, seed.StrategyKey = model.StrategyIdentityBuiltin, req.StrategyKey
		normalized.StrategyKey = req.StrategyKey
	case req.StrategyID > 0:
		strategyID, revisionID := req.StrategyID, resolved.RevisionID
		seed.StrategyIdentity, seed.StrategyID, seed.StrategyRevisionID = model.StrategyIdentityCustom, &strategyID, &revisionID
		normalized.StrategyID, normalized.StrategyRevisionID = strategyID, revisionID
	default:
		seed.StrategyIdentity, seed.StrategyKey = model.StrategyIdentityAdhoc, "adhoc:"+resolved.Hash
	}
	normalized.Tree = resolved.Tree
	return finalizeStrategyRunSeed(seed, normalized)
}

func finalizeStrategyRunSeed(seed strategyRunSeed, request any) (strategyRunSeed, strategyJobSnapshot, error) {
	requestJSON, err := json.Marshal(request)
	if err != nil {
		return strategyRunSeed{}, strategyJobSnapshot{}, err
	}
	if len(requestJSON) == 0 || len(requestJSON) > strategyRunRequestMaxBytes {
		return strategyRunSeed{}, strategyJobSnapshot{}, fmt.Errorf("策略研究请求超过 %d 字节上限", strategyRunRequestMaxBytes)
	}
	sum := sha256.Sum256(requestJSON)
	seed.RequestJSON, seed.RequestHash = string(requestJSON), hex.EncodeToString(sum[:])
	snapshot := strategyJobSnapshot{Version: 1, StrategyIdentity: seed.StrategyIdentity,
		StrategyKey: seed.StrategyKey, StrategyID: pointerValue(seed.StrategyID),
		StrategyRevisionID: pointerValue(seed.StrategyRevisionID), StrategyRevision: seed.StrategyRevision,
		StrategyHash: seed.StrategyHash, RequestHash: seed.RequestHash}
	return seed, snapshot, nil
}

func startStrategyRunJob(userID int64, seed strategyRunSeed, snapshot strategyJobSnapshot) (*StrategyRunView, error) {
	binding := strategyRunSeedBinding(seed)
	run, err := defaultJobRuntime.startWithBinding(userID, seed.Kind, snapshot, false, nil, nil, &binding)
	if err != nil {
		return nil, err
	}
	if run.ResultID == nil {
		return nil, errors.New("策略研究作业缺少结果引用")
	}
	return GetStrategyRun(userID, seed.Kind, *run.ResultID)
}

func ListStrategyRuns(userID int64, kind string, limit int) ([]StrategyRunView, error) {
	if common.DB == nil || userID <= 0 {
		return nil, errors.New("数据库不可用")
	}
	if kind != JobKindScreenerScan && kind != JobKindStrategyBacktest {
		return nil, errors.New("策略研究结果类型无效")
	}
	if limit <= 0 {
		limit = 20
	} else if limit > 100 {
		limit = 100
	}
	var rows []model.StrategyRunResult
	if err := common.DB.Model(&model.StrategyRunResult{}).
		Select("id", "job_run_id", "kind", "strategy_identity", "strategy_key", "strategy_id", "strategy_revision_id",
			"strategy_revision", "strategy_hash", "strategy_name", "request_hash", "as_of", "content_hash", "status",
			"error", "error_code", "started_at", "finished_at", "created_at", "updated_at").
		Where("user_id = ? AND kind = ?", userID, kind).Order("id DESC").Limit(limit).Find(&rows).Error; err != nil {
		return nil, err
	}
	views := make([]StrategyRunView, 0, len(rows))
	for i := range rows {
		views = append(views, strategyRunView(rows[i], false))
	}
	return views, nil
}

func GetStrategyRun(userID int64, kind string, id int64) (*StrategyRunView, error) {
	if common.DB == nil || userID <= 0 || id <= 0 {
		return nil, ErrJobNotFound
	}
	if kind != JobKindScreenerScan && kind != JobKindStrategyBacktest {
		return nil, ErrJobNotFound
	}
	var row model.StrategyRunResult
	if err := common.DB.Where("id = ? AND user_id = ? AND kind = ?", id, userID, kind).First(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrJobNotFound
		}
		return nil, err
	}
	if len(row.RequestJSON) > strategyRunRequestMaxBytes || len(row.ResultJSON) > strategyRunResultMaxBytes {
		return nil, errors.New("策略研究结果正文超过读取边界")
	}
	if row.RequestJSON == "" || sha256Hex([]byte(row.RequestJSON)) != row.RequestHash {
		return nil, errors.New("策略研究规范化请求完整性校验失败")
	}
	if row.Status == model.JobStatusSuccess && (row.ResultJSON == "" || row.ContentHash == "" ||
		sha256Hex([]byte(row.ResultJSON)) != row.ContentHash) {
		return nil, errors.New("策略研究结果正文完整性校验失败")
	}
	view := strategyRunView(row, true)
	return &view, nil
}

func strategyRunView(row model.StrategyRunResult, withBody bool) StrategyRunView {
	view := StrategyRunView{ID: row.ID, JobRunID: row.JobRunID, Kind: row.Kind,
		StrategyIdentity: row.StrategyIdentity, StrategyKey: row.StrategyKey,
		StrategyID: row.StrategyID, StrategyRevisionID: row.StrategyRevisionID,
		StrategyRevision: row.StrategyRevision, StrategyHash: row.StrategyHash, StrategyName: row.StrategyName,
		RequestHash: row.RequestHash, AsOf: row.AsOf, ContentHash: row.ContentHash,
		Status: row.Status, Error: row.Error, ErrorCode: row.ErrorCode,
		StartedAt: row.StartedAt, FinishedAt: row.FinishedAt, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt}
	if withBody {
		view.Request = json.RawMessage(row.RequestJSON)
		if row.Status == model.JobStatusSuccess && row.ResultJSON != "" {
			view.Result = json.RawMessage(row.ResultJSON)
		}
	}
	return view
}

func pointerValue(value *int64) int64 {
	if value == nil {
		return 0
	}
	return *value
}

func sha256Hex(value []byte) string {
	sum := sha256.Sum256(value)
	return hex.EncodeToString(sum[:])
}
