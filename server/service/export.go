package service

import (
	"bytes"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"quantvista/common"
	"quantvista/model"
)

// ExportService 用户数据 CSV 导出与持仓 CSV 导入。
// 导出带 UTF-8 BOM（Excel 直接双击可读中文）；导入逐行校验、错误行报告、不打数据源。
type ExportService struct{}

func NewExportService() *ExportService {
	return &ExportService{}
}

const (
	exportMaxRows    = 5000 // 单次导出行数上限（个人自用远达不到，防御性兜底）
	importMaxRows    = 500  // 单次导入行数上限
	importMaxSize    = 1 << 20
	csvTimeLayout    = "2006-01-02 15:04:05"
	importDateLayout = "2006-01-02"
)

// Export 按 kind 导出当前用户数据，返回（CSV 字节、建议文件名）。
func (s *ExportService) Export(userID int64, kind string) ([]byte, string, error) {
	var rows [][]string
	var err error
	switch kind {
	case "positions":
		rows, err = s.positionRows(userID)
	case "watchlist":
		rows, err = s.watchlistRows(userID)
	case "recommendations":
		rows, err = s.recommendationRows(userID)
	case "analyses":
		rows, err = s.analysisRows(userID)
	default:
		return nil, "", errors.New("不支持的导出类型")
	}
	if err != nil {
		return nil, "", err
	}
	name := fmt.Sprintf("%s-%s.csv", kind, time.Now().Format("20060102"))
	return writeCSV(rows), name, nil
}

// writeCSV 编码为带 UTF-8 BOM 的 CSV（Excel 双击打开不乱码）。
func writeCSV(rows [][]string) []byte {
	var buf bytes.Buffer
	buf.WriteString("\xEF\xBB\xBF")
	w := csv.NewWriter(&buf)
	_ = w.WriteAll(rows)
	w.Flush()
	return buf.Bytes()
}

func f2(v float64) string {
	return strconv.FormatFloat(v, 'f', -1, 64)
}

func (s *ExportService) positionRows(userID int64) ([][]string, error) {
	var ps []model.Position
	if err := common.DB.Where("user_id = ?", userID).Order("id ASC").Limit(exportMaxRows).Find(&ps).Error; err != nil {
		return nil, err
	}
	rows := [][]string{{
		"id", "symbol", "market", "name", "type", "status", "currency",
		"buy_price", "buy_date", "quantity", "buy_fee", "buy_tax", "buy_reason",
		"plan_stop_loss", "plan_take_profit",
		"sell_price", "sell_date", "sell_fee", "sell_tax", "sell_reason", "review_note",
		"recommendation_id", "created_at",
	}}
	for _, p := range ps {
		rows = append(rows, []string{
			strconv.FormatInt(p.ID, 10), p.Symbol, p.Market, p.Name, p.PositionType, p.Status, p.Currency,
			f2(p.BuyPrice), p.BuyDate, f2(p.Quantity), f2(p.BuyFee), f2(p.BuyTax), p.BuyReason,
			f2(p.PlanStopLoss), f2(p.PlanTakeProfit),
			f2(p.SellPrice), p.SellDate, f2(p.SellFee), f2(p.SellTax), p.SellReason, p.ReviewNote,
			strconv.FormatInt(p.RecommendationID, 10), p.CreatedAt.Format(csvTimeLayout),
		})
	}
	return rows, nil
}

func (s *ExportService) watchlistRows(userID int64) ([][]string, error) {
	var lists []model.Watchlist
	if err := common.DB.Where("user_id = ?", userID).Find(&lists).Error; err != nil {
		return nil, err
	}
	groupName := make(map[int64]string, len(lists))
	for _, l := range lists {
		groupName[l.ID] = l.Name
	}
	var items []model.WatchlistItem
	if err := common.DB.Where("user_id = ?", userID).Order("id ASC").Limit(exportMaxRows).Find(&items).Error; err != nil {
		return nil, err
	}
	rows := [][]string{{
		"group", "symbol", "market", "name", "is_pinned", "research_stage",
		"note", "focus_reason", "passed_reason", "created_at",
	}}
	for _, it := range items {
		rows = append(rows, []string{
			groupName[it.WatchlistID], it.Symbol, it.Market, it.Name,
			strconv.FormatBool(it.IsPinned), it.ResearchStage,
			it.Note, it.FocusReason, it.PassedReason, it.CreatedAt.Format(csvTimeLayout),
		})
	}
	return rows, nil
}

func (s *ExportService) recommendationRows(userID int64) ([][]string, error) {
	var batches []model.RecommendationBatch
	if err := common.DB.Where("user_id = ?", userID).Find(&batches).Error; err != nil {
		return nil, err
	}
	batchOf := make(map[int64]model.RecommendationBatch, len(batches))
	for _, b := range batches {
		batchOf[b.ID] = b
	}
	var recs []model.Recommendation
	if err := common.DB.Where("user_id = ?", userID).Order("id ASC").Limit(exportMaxRows).Find(&recs).Error; err != nil {
		return nil, err
	}
	rows := [][]string{{
		"batch_id", "batch_type", "strategy", "market", "symbol", "name",
		"action", "confidence", "ref_price", "summary", "created_at",
	}}
	for _, r := range recs {
		b := batchOf[r.BatchID]
		rows = append(rows, []string{
			strconv.FormatInt(r.BatchID, 10), b.Type, b.Strategy, b.Market, r.Symbol, r.Name,
			r.Action, strconv.Itoa(r.Confidence), f2(r.RefPrice), r.Summary,
			r.CreatedAt.Format(csvTimeLayout),
		})
	}
	return rows, nil
}

func (s *ExportService) analysisRows(userID int64) ([][]string, error) {
	var recs []model.AnalysisRecord
	if err := common.DB.Where("user_id = ?", userID).
		Select("id", "module", "mode", "market", "symbol", "target", "title", "status",
			"rating", "confidence", "summary", "prompt_version", "model", "total_tokens", "created_at").
		Order("id ASC").Limit(exportMaxRows).Find(&recs).Error; err != nil {
		return nil, err
	}
	rows := [][]string{{
		"id", "module", "mode", "market", "symbol", "target", "title", "status",
		"rating", "confidence", "summary", "prompt_version", "model", "total_tokens", "created_at",
	}}
	for _, r := range recs {
		rows = append(rows, []string{
			strconv.FormatInt(r.ID, 10), r.Module, r.Mode, r.Market, r.Symbol, r.Target, r.Title, r.Status,
			r.Rating, strconv.Itoa(r.Confidence), r.Summary, r.PromptVersion, r.Model,
			strconv.Itoa(r.TotalTokens), r.CreatedAt.Format(csvTimeLayout),
		})
	}
	return rows, nil
}

// --- 持仓 CSV 导入 ---

// ImportRowError 单行导入失败的报告。
type ImportRowError struct {
	Row   int    `json:"row"` // 数据行号（含表头的物理行号，从 2 起）
	Error string `json:"error"`
}

// ImportResult 导入结果：成功条数 + 错误行明细。
type ImportResult struct {
	Imported int              `json:"imported"`
	Failed   []ImportRowError `json:"failed"`
}

// PositionImportTemplate 导入模板表头（与导出列名对齐的最小买入集）。
var PositionImportTemplate = []string{"symbol", "market", "type", "buy_price", "buy_date", "quantity", "buy_fee", "buy_tax", "reason"}

// ImportPositions 从 CSV 导入持仓。逐行校验，坏行跳过并报告，好行全部入库。
// 不逐行打行情源取名（500 行会打爆免费源），name 记 symbol、由列表页行情富化展示。
func (s *ExportService) ImportPositions(userID int64, r io.Reader) (*ImportResult, error) {
	reader := csv.NewReader(io.LimitReader(r, importMaxSize))
	reader.FieldsPerRecord = -1 // 行内列数自适应，逐行校验
	reader.TrimLeadingSpace = true

	header, err := reader.Read()
	if err != nil {
		return nil, errors.New("空文件或无法解析 CSV")
	}
	if len(header) > 0 {
		header[0] = strings.TrimPrefix(header[0], "\xEF\xBB\xBF") // 容忍 BOM
	}
	col := map[string]int{}
	for i, h := range header {
		col[strings.ToLower(strings.TrimSpace(h))] = i
	}
	for _, need := range []string{"symbol", "buy_price", "buy_date", "quantity"} {
		if _, ok := col[need]; !ok {
			return nil, fmt.Errorf("缺少必需列 %s（模板列：%s）", need, strings.Join(PositionImportTemplate, ","))
		}
	}
	get := func(rec []string, name string) string {
		if i, ok := col[name]; ok && i < len(rec) {
			return strings.TrimSpace(rec[i])
		}
		return ""
	}

	res := &ImportResult{Failed: []ImportRowError{}}
	var pending []model.Position
	line := 1
	for {
		rec, rerr := reader.Read()
		if rerr == io.EOF {
			break
		}
		line++
		if rerr != nil {
			res.Failed = append(res.Failed, ImportRowError{Row: line, Error: "CSV 解析失败：" + rerr.Error()})
			continue
		}
		if len(pending)+1 > importMaxRows {
			return nil, fmt.Errorf("超出单次导入上限 %d 行，请分批导入", importMaxRows)
		}
		p, perr := parseImportRow(userID, rec, get)
		if perr != nil {
			res.Failed = append(res.Failed, ImportRowError{Row: line, Error: perr.Error()})
			continue
		}
		pending = append(pending, *p)
	}
	if len(pending) == 0 {
		return res, nil
	}
	if err := common.DB.CreateInBatches(pending, 100).Error; err != nil {
		return nil, err
	}
	res.Imported = len(pending)
	return res, nil
}

// parseImportRow 校验并组装单行持仓。
func parseImportRow(userID int64, rec []string, get func([]string, string) string) (*model.Position, error) {
	symbol, market, err := normalizeSymbolMarket(get(rec, "symbol"), orDefaultStr(get(rec, "market"), "cn"))
	if err != nil {
		return nil, err
	}
	ptype := strings.ToLower(get(rec, "type"))
	switch ptype {
	case "", "long_term", "长线":
		ptype = model.PositionTypeLongTerm
	case "short_term", "短线":
		ptype = model.PositionTypeShortTerm
	default:
		return nil, fmt.Errorf("type 须为 short_term/long_term，得到 %q", get(rec, "type"))
	}
	buyPrice, err := strconv.ParseFloat(get(rec, "buy_price"), 64)
	if err != nil || buyPrice <= 0 {
		return nil, errors.New("buy_price 必须为正数")
	}
	qty, err := strconv.ParseFloat(get(rec, "quantity"), 64)
	if err != nil || qty <= 0 {
		return nil, errors.New("quantity 必须为正数")
	}
	buyDate := get(rec, "buy_date")
	if _, err := time.ParseInLocation(importDateLayout, buyDate, time.Local); err != nil {
		return nil, errors.New("buy_date 须为 YYYY-MM-DD 格式")
	}
	fee, tax := 0.0, 0.0
	if v := get(rec, "buy_fee"); v != "" {
		if fee, err = strconv.ParseFloat(v, 64); err != nil || fee < 0 {
			return nil, errors.New("buy_fee 须为非负数")
		}
	}
	if v := get(rec, "buy_tax"); v != "" {
		if tax, err = strconv.ParseFloat(v, 64); err != nil || tax < 0 {
			return nil, errors.New("buy_tax 须为非负数")
		}
	}
	return &model.Position{
		UserID: userID, Symbol: symbol, Market: market, Name: symbol,
		PositionType: ptype, Status: model.PositionStatusHolding,
		Currency: defaultCurrencyFor(market),
		BuyPrice: buyPrice, BuyDate: buyDate, Quantity: qty, BuyFee: fee, BuyTax: tax,
		BuyReason: truncateRunes(get(rec, "reason"), 500),
	}, nil
}

func orDefaultStr(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}
