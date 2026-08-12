package service

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"quantvista/common"
	"quantvista/model"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	dataImportMaxBytes  = 1 << 20
	dataImportMaxRows   = 500
	dataImportMaxFields = 32
	dataImportMaxField  = 2048
	dataImportMaxNumber = 1e15
	dataImportVersion   = 1
)

type DataImportService struct{}

var dataImportMutationMu sync.Mutex

func NewDataImportService() *DataImportService { return &DataImportService{} }

type ImportColumn struct {
	Key      string `json:"key"`
	Label    string `json:"label"`
	Required bool   `json:"required"`
}

type ImportRowView struct {
	Row        int               `json:"row"`
	Status     string            `json:"status"`
	ErrorCode  string            `json:"error_code,omitempty"`
	Message    string            `json:"message,omitempty"`
	Raw        map[string]string `json:"raw"`
	Normalized map[string]any    `json:"normalized,omitempty"`
}

type ImportBatchView struct {
	model.ImportBatch
	Headers     []string          `json:"headers"`
	Columns     []ImportColumn    `json:"columns"`
	Suggestions map[string]string `json:"suggestions"`
	Mapping     map[string]string `json:"mapping"`
	Rows        []ImportRowView   `json:"rows"`
}

type ImportMappingInput struct {
	Version       int               `json:"version"`
	Mapping       map[string]string `json:"mapping"`
	TargetGroupID int64             `json:"target_group_id"`
}

type ImportConfirmInput struct {
	Version int `json:"version"`
}

type ImportRollbackConflict struct {
	RecordKind string `json:"record_kind"`
	RecordID   int64  `json:"record_id"`
	Message    string `json:"message"`
}

type ImportRollbackResult struct {
	BatchID   string                   `json:"batch_id"`
	Status    string                   `json:"status"`
	Conflicts []ImportRollbackConflict `json:"conflicts"`
}

type importFieldDef struct {
	Key      string
	Label    string
	Required bool
	Aliases  []string
}

var importDefinitions = map[string][]importFieldDef{
	model.ImportKindWatchlist: {
		{Key: "symbol", Label: "股票代码", Required: true, Aliases: []string{"symbol", "code", "stock_code", "股票代码", "证券代码", "代码"}},
		{Key: "market", Label: "市场", Aliases: []string{"market", "exchange", "市场", "交易市场", "交易所"}},
		{Key: "name", Label: "股票名称", Aliases: []string{"name", "stock_name", "股票名称", "证券名称", "名称"}},
		{Key: "note", Label: "备注", Aliases: []string{"note", "remark", "备注", "说明"}},
		{Key: "focus_reason", Label: "关注原因", Aliases: []string{"focus_reason", "reason", "关注原因", "关注理由"}},
	},
	model.ImportKindPosition: {
		{Key: "symbol", Label: "股票代码", Required: true, Aliases: []string{"symbol", "code", "stock_code", "股票代码", "证券代码", "代码"}},
		{Key: "market", Label: "市场", Aliases: []string{"market", "exchange", "市场", "交易市场", "交易所"}},
		{Key: "name", Label: "股票名称", Aliases: []string{"name", "stock_name", "股票名称", "证券名称", "名称"}},
		{Key: "position_type", Label: "持仓类型", Aliases: []string{"position_type", "type", "持仓类型", "类型"}},
		{Key: "price", Label: "买入价格", Required: true, Aliases: []string{"price", "buy_price", "cost_price", "买入价格", "成本价", "成交价"}},
		{Key: "quantity", Label: "数量", Required: true, Aliases: []string{"quantity", "qty", "volume", "数量", "成交数量", "持仓数量"}},
		{Key: "trade_date", Label: "买入日期", Required: true, Aliases: []string{"trade_date", "buy_date", "date", "买入日期", "成交日期", "日期"}},
		{Key: "fee", Label: "手续费", Aliases: []string{"fee", "buy_fee", "commission", "手续费", "佣金"}},
		{Key: "tax", Label: "税费", Aliases: []string{"tax", "buy_tax", "stamp_tax", "税费", "印花税"}},
		{Key: "note", Label: "买入理由", Aliases: []string{"note", "reason", "buy_reason", "买入理由", "备注"}},
	},
	model.ImportKindTrade: {
		{Key: "position_id", Label: "持仓 ID", Aliases: []string{"position_id", "positionid", "持仓id", "持仓ID"}},
		{Key: "symbol", Label: "股票代码", Required: true, Aliases: []string{"symbol", "code", "stock_code", "股票代码", "证券代码", "代码"}},
		{Key: "market", Label: "市场", Aliases: []string{"market", "exchange", "市场", "交易市场", "交易所"}},
		{Key: "name", Label: "股票名称", Aliases: []string{"name", "stock_name", "股票名称", "证券名称", "名称"}},
		{Key: "side", Label: "买卖方向", Required: true, Aliases: []string{"side", "direction", "bs", "买卖方向", "方向", "买卖"}},
		{Key: "quantity", Label: "成交数量", Required: true, Aliases: []string{"quantity", "qty", "volume", "成交数量", "数量"}},
		{Key: "price", Label: "成交价格", Required: true, Aliases: []string{"price", "trade_price", "成交价格", "成交价", "价格"}},
		{Key: "trade_date", Label: "成交日期", Required: true, Aliases: []string{"trade_date", "date", "成交日期", "交易日期", "日期"}},
		{Key: "fee", Label: "手续费", Aliases: []string{"fee", "commission", "手续费", "佣金"}},
		{Key: "tax", Label: "税费", Aliases: []string{"tax", "stamp_tax", "税费", "印花税"}},
		{Key: "note", Label: "备注", Aliases: []string{"note", "remark", "备注", "说明"}},
	},
}

var (
	cnImportSymbol = regexp.MustCompile(`^[0-9]{6}$`)
	hkImportSymbol = regexp.MustCompile(`^[0-9]{5}$`)
	usImportSymbol = regexp.MustCompile(`^[A-Z][A-Z0-9.-]{0,9}$`)
)

type importNormalized struct {
	Symbol         string  `json:"symbol"`
	Market         string  `json:"market"`
	Name           string  `json:"name,omitempty"`
	PositionType   string  `json:"position_type,omitempty"`
	PositionID     int64   `json:"position_id,omitempty"`
	VirtualKey     string  `json:"virtual_key,omitempty"`
	Side           string  `json:"side,omitempty"`
	Price          float64 `json:"price,omitempty"`
	Quantity       float64 `json:"quantity,omitempty"`
	TradeDate      string  `json:"trade_date,omitempty"`
	Fee            float64 `json:"fee,omitempty"`
	Tax            float64 `json:"tax,omitempty"`
	Note           string  `json:"note,omitempty"`
	FocusReason    string  `json:"focus_reason,omitempty"`
	DependencyHash string  `json:"dependency_hash,omitempty"`
}

func normalizeImportKind(kind string) (string, error) {
	kind = strings.ToLower(strings.TrimSpace(kind))
	switch kind {
	case model.ImportKindWatchlist, "watchlists", "自选":
		return model.ImportKindWatchlist, nil
	case model.ImportKindPosition, "positions", "持仓":
		return model.ImportKindPosition, nil
	case model.ImportKindTrade, "trades", "成交流水", "成交":
		return model.ImportKindTrade, nil
	default:
		return "", errors.New("导入类型仅支持自选、持仓或成交流水")
	}
}

func newImportID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	s := hex.EncodeToString(b)
	return s[:8] + "-" + s[8:12] + "-" + s[12:16] + "-" + s[16:20] + "-" + s[20:], nil
}

func hashBytes(v []byte) string { h := sha256.Sum256(v); return hex.EncodeToString(h[:]) }

func canonicalJSON(v any) ([]byte, error) { return json.Marshal(v) }

func importColumns(kind string) []ImportColumn {
	defs := importDefinitions[kind]
	out := make([]ImportColumn, 0, len(defs))
	for _, d := range defs {
		out = append(out, ImportColumn{Key: d.Key, Label: d.Label, Required: d.Required})
	}
	return out
}

func suggestImportMapping(kind string, headers []string) map[string]string {
	byNormalized := make(map[string]string, len(headers))
	for _, h := range headers {
		byNormalized[strings.ToLower(strings.TrimSpace(h))] = h
	}
	out := map[string]string{}
	for _, d := range importDefinitions[kind] {
		for _, alias := range d.Aliases {
			if h, ok := byNormalized[strings.ToLower(alias)]; ok {
				out[d.Key] = h
				break
			}
		}
	}
	return out
}

func (s *DataImportService) Upload(userID int64, kind, fileName string, r io.Reader) (*ImportBatchView, error) {
	return s.UploadByAccount(userID, 0, kind, fileName, r)
}

func (s *DataImportService) UploadByAccount(userID, requestedAccountID int64, kind, fileName string, r io.Reader) (*ImportBatchView, error) {
	if common.DB == nil {
		return nil, errors.New("数据库不可用")
	}
	var err error
	kind, err = normalizeImportKind(kind)
	if err != nil {
		return nil, err
	}
	accountID := int64(0)
	if kind == model.ImportKindPosition || kind == model.ImportKindTrade {
		account, accountErr := ResolvePortfolioAccount(userID, requestedAccountID, model.PortfolioKindReal)
		if accountErr != nil {
			return nil, errors.New("组合不存在")
		}
		if account.Status != model.PortfolioStatusActive {
			return nil, errors.New("组合已归档，仅允许读取历史数据")
		}
		accountID = account.ID
	}
	limited := io.LimitReader(r, dataImportMaxBytes+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		return nil, errors.New("读取 CSV 失败")
	}
	if len(data) == 0 {
		return nil, errors.New("CSV 文件为空")
	}
	if len(data) > dataImportMaxBytes {
		return nil, fmt.Errorf("CSV 文件不能超过 %d MiB", dataImportMaxBytes>>20)
	}
	data = bytes.TrimPrefix(data, []byte{0xef, 0xbb, 0xbf})
	if !utf8.Valid(data) || bytes.IndexByte(data, 0) >= 0 {
		return nil, errors.New("CSV 必须使用 UTF-8 或 UTF-8 BOM 编码")
	}
	digest := hashBytes(data)
	var existing model.ImportBatch
	attempt := 1
	if err := common.DB.Where("user_id = ? AND account_id = ? AND kind = ? AND file_digest = ?", userID, accountID, kind, digest).Order("attempt DESC, created_at DESC").First(&existing).Error; err == nil {
		if existing.Status != model.ImportStatusRolledBack {
			return s.Get(userID, existing.ID)
		}
		attempt = existing.Attempt + 1
	} else if !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, err
	}

	reader := csv.NewReader(bytes.NewReader(data))
	reader.FieldsPerRecord = -1
	reader.TrimLeadingSpace = true
	headers, err := reader.Read()
	if err != nil || len(headers) == 0 {
		return nil, errors.New("CSV 表头无法解析")
	}
	if len(headers) > dataImportMaxFields {
		return nil, fmt.Errorf("CSV 列数不能超过 %d", dataImportMaxFields)
	}
	seenHeaders := map[string]bool{}
	for i := range headers {
		headers[i] = strings.TrimSpace(strings.TrimPrefix(headers[i], "\ufeff"))
		key := strings.ToLower(headers[i])
		if headers[i] == "" || utf8.RuneCountInString(headers[i]) > 128 {
			return nil, errors.New("CSV 表头含空列名或超长列名")
		}
		if formulaLikeImportCell(headers[i]) {
			return nil, errors.New("CSV 表头含可能被表格软件执行的公式内容")
		}
		if seenHeaders[key] {
			return nil, fmt.Errorf("CSV 存在重复列名：%s", headers[i])
		}
		seenHeaders[key] = true
	}

	id, err := newImportID()
	if err != nil {
		return nil, errors.New("生成导入批次失败")
	}
	headerJSON, _ := json.Marshal(headers)
	batch := model.ImportBatch{
		ID: id, UserID: userID, AccountID: accountID, Kind: kind, SchemaVersion: dataImportVersion, Version: 1,
		Attempt: attempt, Status: model.ImportStatusUploaded, FileName: truncateRunes(strings.TrimSpace(fileName), 250),
		FileDigest: digest, HeaderJSON: string(headerJSON),
	}
	rows := make([]model.ImportRow, 0)
	physicalRow := 1
	for {
		record, readErr := reader.Read()
		if readErr == io.EOF {
			break
		}
		physicalRow++
		if readErr != nil {
			return nil, fmt.Errorf("CSV 第 %d 行格式无法解析", physicalRow)
		}
		if len(rows) >= dataImportMaxRows {
			return nil, fmt.Errorf("单批最多导入 %d 行，请拆分文件", dataImportMaxRows)
		}
		if len(record) > len(headers) {
			return nil, fmt.Errorf("CSV 第 %d 行列数超过表头", physicalRow)
		}
		for len(record) < len(headers) {
			record = append(record, "")
		}
		for _, cell := range record {
			if utf8.RuneCountInString(cell) > dataImportMaxField {
				return nil, fmt.Errorf("CSV 第 %d 行存在超过 %d 字的字段", physicalRow, dataImportMaxField)
			}
		}
		rawJSON, _ := json.Marshal(record)
		rows = append(rows, model.ImportRow{
			UserID: userID, BatchID: id, RowNumber: physicalRow,
			RowDigest: hashBytes(rawJSON), RawJSON: string(rawJSON), Status: model.ImportStatusUploaded,
		})
	}
	if len(rows) == 0 {
		return nil, errors.New("CSV 没有数据行")
	}
	batch.TotalRows = len(rows)
	if err := common.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&batch).Error; err != nil {
			return err
		}
		return tx.CreateInBatches(rows, 100).Error
	}); err != nil {
		// 并发重复上传以 user+kind+文件摘要+尝试次数的唯一索引收敛；已回滚的旧尝试不复用。
		if e := common.DB.Where("user_id = ? AND account_id = ? AND kind = ? AND file_digest = ?", userID, accountID, kind, digest).Order("attempt DESC, created_at DESC").First(&existing).Error; e == nil && existing.Attempt >= attempt && existing.Status != model.ImportStatusRolledBack {
			return s.Get(userID, existing.ID)
		}
		return nil, err
	}
	return s.Get(userID, id)
}

func decodeHeaders(batch model.ImportBatch) ([]string, error) {
	var headers []string
	if err := json.Unmarshal([]byte(batch.HeaderJSON), &headers); err != nil {
		return nil, errors.New("导入批次表头损坏")
	}
	return headers, nil
}

func (s *DataImportService) Get(userID int64, batchID string) (*ImportBatchView, error) {
	var batch model.ImportBatch
	if err := common.DB.Where("id = ? AND user_id = ?", strings.TrimSpace(batchID), userID).First(&batch).Error; err != nil {
		return nil, errors.New("导入批次不存在")
	}
	headers, err := decodeHeaders(batch)
	if err != nil {
		return nil, err
	}
	var mapping map[string]string
	if batch.MappingJSON != "" {
		_ = json.Unmarshal([]byte(batch.MappingJSON), &mapping)
	}
	if mapping == nil {
		mapping = map[string]string{}
	}
	var rows []model.ImportRow
	if err := common.DB.Where("batch_id = ? AND user_id = ?", batch.ID, userID).Order("row_number ASC").Find(&rows).Error; err != nil {
		return nil, err
	}
	views := make([]ImportRowView, 0, len(rows))
	for _, row := range rows {
		var cells []string
		_ = json.Unmarshal([]byte(row.RawJSON), &cells)
		raw := map[string]string{}
		for i, h := range headers {
			if i < len(cells) {
				raw[h] = cells[i]
			}
		}
		var normalized map[string]any
		if row.NormalizedJSON != "" {
			_ = json.Unmarshal([]byte(row.NormalizedJSON), &normalized)
			delete(normalized, "dependency_hash")
		}
		views = append(views, ImportRowView{Row: row.RowNumber, Status: row.Status, ErrorCode: row.ErrorCode, Message: row.Message, Raw: raw, Normalized: normalized})
	}
	return &ImportBatchView{
		ImportBatch: batch, Headers: headers, Columns: importColumns(batch.Kind),
		Suggestions: suggestImportMapping(batch.Kind, headers), Mapping: mapping, Rows: views,
	}, nil
}

// List 返回最近批次的轻量审计摘要，不读取原始行与规范化正文。
func (s *DataImportService) List(userID int64, limit int) ([]model.ImportBatch, error) {
	if limit <= 0 || limit > 50 {
		limit = 20
	}
	var rows []model.ImportBatch
	if err := common.DB.Select("id", "user_id", "account_id", "kind", "schema_version", "attempt", "version", "status", "file_name", "file_digest", "mapping_digest", "target_group_id", "total_rows", "valid_rows", "error_rows", "conflict_rows", "created_rows", "updated_rows", "confirmed_at", "rolled_back_at", "created_at", "updated_at").Where("user_id = ?", userID).Order("created_at DESC, id DESC").Limit(limit).Find(&rows).Error; err != nil {
		return nil, err
	}
	if rows == nil {
		rows = []model.ImportBatch{}
	}
	return rows, nil
}

func validateImportMapping(kind string, headers []string, mapping map[string]string) (map[string]int, error) {
	knownFields := map[string]importFieldDef{}
	for _, d := range importDefinitions[kind] {
		knownFields[d.Key] = d
	}
	headerIndex := map[string]int{}
	for i, h := range headers {
		headerIndex[h] = i
	}
	indexes := map[string]int{}
	used := map[int]string{}
	for key, header := range mapping {
		key = strings.TrimSpace(key)
		def, ok := knownFields[key]
		if !ok {
			return nil, fmt.Errorf("未知映射字段：%s", key)
		}
		_ = def
		header = strings.TrimSpace(header)
		if header == "" {
			continue
		}
		idx, ok := headerIndex[header]
		if !ok {
			return nil, fmt.Errorf("CSV 中不存在列：%s", header)
		}
		if previous, exists := used[idx]; exists {
			return nil, fmt.Errorf("同一 CSV 列不能同时映射到 %s 和 %s", previous, key)
		}
		used[idx] = key
		indexes[key] = idx
	}
	for _, d := range importDefinitions[kind] {
		if d.Required {
			if _, ok := indexes[d.Key]; !ok {
				return nil, fmt.Errorf("请映射必填字段：%s", d.Label)
			}
		}
	}
	return indexes, nil
}

func formulaLikeImportCell(v string) bool {
	t := strings.TrimLeftFunc(v, unicode.IsSpace)
	if t == "" {
		return false
	}
	if t[0] == '\t' || t[0] == '\r' || t[0] == '=' || t[0] == '@' {
		return true
	}
	if t[0] == '+' || t[0] == '-' {
		_, err := strconv.ParseFloat(t, 64)
		return err != nil
	}
	return false
}

func validateImportSymbol(symbol, market string) (string, string, error) {
	symbol, market, err := normalizeSymbolMarket(symbol, market)
	if err != nil {
		return "", "", err
	}
	switch market {
	case "cn":
		if !cnImportSymbol.MatchString(symbol) {
			return "", "", errors.New("A 股代码须为 6 位数字")
		}
	case "hk":
		if !hkImportSymbol.MatchString(symbol) {
			return "", "", errors.New("港股代码须为 5 位数字")
		}
	case "us":
		symbol = strings.ToUpper(symbol)
		if !usImportSymbol.MatchString(symbol) {
			return "", "", errors.New("美股代码格式无效")
		}
	}
	return symbol, market, nil
}

func importCell(cells []string, indexes map[string]int, key string) string {
	if i, ok := indexes[key]; ok && i < len(cells) {
		return strings.TrimSpace(cells[i])
	}
	return ""
}

func parseImportNumber(v, label string, required bool) (float64, error) {
	if strings.TrimSpace(v) == "" {
		if required {
			return 0, fmt.Errorf("%s不能为空", label)
		}
		return 0, nil
	}
	n, err := strconv.ParseFloat(v, 64)
	if err != nil || math.IsNaN(n) || math.IsInf(n, 0) || n < 0 {
		return 0, fmt.Errorf("%s须为非负数", label)
	}
	if n > dataImportMaxNumber {
		return 0, fmt.Errorf("%s数值过大", label)
	}
	if required && n <= 0 {
		return 0, fmt.Errorf("%s必须大于 0", label)
	}
	return round4(n), nil
}

func parseImportSide(v string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "buy", "b", "买", "买入":
		return model.PositionTradeBuy, nil
	case "sell", "s", "卖", "卖出":
		return model.PositionTradeSell, nil
	default:
		return "", errors.New("买卖方向必须明确为买入或卖出")
	}
}

func parseImportDate(v string) (string, error) {
	v = strings.TrimSpace(v)
	if _, err := time.Parse("2006-01-02", v); err != nil {
		return "", errors.New("成交日期须为 YYYY-MM-DD 格式")
	}
	if v > time.Now().In(time.Local).Format("2006-01-02") {
		return "", errors.New("成交日期不能晚于今天")
	}
	return v, nil
}

func importPositionType(v string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "", "long_term", "long", "长线", "长期":
		return model.PositionTypeLongTerm, nil
	case "short_term", "short", "短线", "短期":
		return model.PositionTypeShortTerm, nil
	default:
		return "", errors.New("持仓类型须为短线或长线")
	}
}

func importIssueStatus(code string) string {
	switch code {
	case "already_exists", "possible_duplicate", "ambiguous_position", "position_not_found", "position_closed", "position_mismatch", "out_of_order", "ledger_conflict":
		return model.ImportRowConflict
	default:
		return model.ImportRowError
	}
}

func positionImportFingerprint(p model.Position) string {
	p.CreatedAt, p.UpdatedAt = time.Time{}, time.Time{}
	// 峰值三列由 16:25 盘后任务自动抬升，不是账本语义：留在指纹里会让“导入后任一
	// 交易日创新高”的持仓永久不可回滚，且把系统后台写入误归因为“后续交易或人工
	// 编辑”。回滚后峰值可由盘后任务重建，剔除无损。
	p.PeakPrice, p.PeakDate, p.PeakBackfilled = 0, "", false
	b, _ := json.Marshal(p)
	return hashBytes(b)
}

func watchlistImportFingerprint(item model.WatchlistItem) string {
	item.CreatedAt, item.UpdatedAt = time.Time{}, time.Time{}
	b, _ := json.Marshal(item)
	return hashBytes(b)
}

func importIdempotencyDigest(n importNormalized) string {
	// 目标持仓 ID、虚拟键和预检依赖会随账本变化；它们不属于成交本身。没有券商订单号
	// 时，以标的+方向+数量+价格+日期+费税+用户备注做保守幂等身份。
	n.PositionID, n.VirtualKey, n.DependencyHash = 0, "", ""
	b, _ := canonicalJSON(n)
	return hashBytes(b)
}

type importPositionSnapshot struct {
	Position    model.Position `json:"position"`
	LastTradeID int64          `json:"last_trade_id"`
}

func positionDependency(db *gorm.DB, p model.Position) (importPositionSnapshot, string, error) {
	var last model.PositionTrade
	err := db.Where("user_id = ? AND position_id = ?", p.UserID, p.ID).Order("id DESC").First(&last).Error
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return importPositionSnapshot{}, "", err
	}
	snap := importPositionSnapshot{Position: p, LastTradeID: last.ID}
	b, _ := json.Marshal(struct {
		Fingerprint string `json:"fingerprint"`
		LastTradeID int64  `json:"last_trade_id"`
	}{positionImportFingerprint(p), last.ID})
	return snap, hashBytes(b), nil
}

type importPreviewState struct {
	Position       model.Position
	Ledger         positionLedger
	LastTradeDate  string
	DependencyHash string
	VirtualKey     string
	ClosedInBatch  bool
}

func (s *DataImportService) Preview(userID int64, batchID string, in ImportMappingInput) (*ImportBatchView, error) {
	if in.Version <= 0 {
		return nil, errors.New("缺少有效的批次版本")
	}
	var batch model.ImportBatch
	if err := common.DB.Where("id = ? AND user_id = ?", batchID, userID).First(&batch).Error; err != nil {
		return nil, errors.New("导入批次不存在")
	}
	if batch.Status == model.ImportStatusConfirmed || batch.Status == model.ImportStatusRolledBack {
		return nil, errors.New("已确认或已回滚的批次不能重新预检")
	}
	if batch.Version != in.Version {
		return nil, errors.New("批次已变化，请刷新后重试")
	}
	headers, err := decodeHeaders(batch)
	if err != nil {
		return nil, err
	}
	indexes, err := validateImportMapping(batch.Kind, headers, in.Mapping)
	if err != nil {
		return nil, err
	}
	if batch.Kind == model.ImportKindWatchlist {
		var group model.Watchlist
		if in.TargetGroupID <= 0 || common.DB.Where("id = ? AND user_id = ?", in.TargetGroupID, userID).First(&group).Error != nil {
			return nil, errors.New("请选择本人有效的自选分组")
		}
	} else {
		in.TargetGroupID = 0
	}
	var rows []model.ImportRow
	if err := common.DB.Where("batch_id = ? AND user_id = ?", batch.ID, userID).Order("row_number ASC").Find(&rows).Error; err != nil {
		return nil, err
	}
	type result struct {
		normalized            importNormalized
		status, code, message string
	}
	results := make([]result, len(rows))
	states := map[string]*importPreviewState{}
	duplicateGroups := map[string][]int{}
	for i, row := range rows {
		results[i].status = model.ImportRowValid
		var cells []string
		if err := json.Unmarshal([]byte(row.RawJSON), &cells); err != nil {
			results[i] = result{status: model.ImportRowError, code: "row_corrupt", message: "原始行事实损坏"}
			continue
		}
		for _, cell := range cells {
			if formulaLikeImportCell(cell) {
				results[i] = result{status: model.ImportRowError, code: "formula_injection", message: "检测到可能被表格软件执行的公式内容"}
				break
			}
		}
		if results[i].status == model.ImportRowError {
			continue
		}
		n, code, message := s.normalizePreviewRow(userID, batch.AccountID, batch.Kind, in.TargetGroupID, cells, indexes, states)
		results[i].normalized = n
		if message != "" {
			results[i].status, results[i].code, results[i].message = importIssueStatus(code), code, message
			continue
		}
		digest := importIdempotencyDigest(n)
		duplicateGroups[digest] = append(duplicateGroups[digest], i)
	}
	for _, indexes := range duplicateGroups {
		if len(indexes) <= 1 {
			continue
		}
		for _, i := range indexes {
			results[i].status = model.ImportRowConflict
			results[i].code = "duplicate_row"
			results[i].message = "批次内存在完全重复的行，请只保留一行"
		}
	}
	// 已确认行声明跨文件、跨映射生效。批次内重复先报 duplicate_row，避免被历史冲突覆盖。
	digests := make([]string, 0, len(duplicateGroups))
	for digest := range duplicateGroups {
		digests = append(digests, digest)
	}
	var claims []model.ImportRowClaim
	if len(digests) > 0 {
		if err := common.DB.Where("user_id = ? AND account_id = ? AND kind = ? AND row_digest IN ?", userID, batch.AccountID, batch.Kind, digests).Find(&claims).Error; err != nil {
			return nil, err
		}
	}
	claimed := map[string]bool{}
	for _, claim := range claims {
		claimed[claim.RowDigest] = true
	}
	for digest, indexes := range duplicateGroups {
		if !claimed[digest] || len(indexes) != 1 {
			continue
		}
		i := indexes[0]
		results[i].status, results[i].code, results[i].message = model.ImportRowConflict, "previously_imported", "该业务行已在其他确认批次中导入"
	}

	mappingJSON, _ := canonicalJSON(in.Mapping)
	mappingDigest := hashBytes(mappingJSON)
	valid, invalid, conflicts := 0, 0, 0
	err = common.DB.Transaction(func(tx *gorm.DB) error {
		var locked model.ImportBatch
		q := tx.Where("id = ? AND user_id = ?", batch.ID, userID)
		if !common.UsingSQLite {
			q = q.Clauses(clause.Locking{Strength: "UPDATE"})
		}
		if err := q.First(&locked).Error; err != nil {
			return errors.New("导入批次不存在")
		}
		if locked.Version != in.Version || (locked.Status != model.ImportStatusUploaded && locked.Status != model.ImportStatusPreviewed) {
			return errors.New("批次已变化，请刷新后重试")
		}
		for i := range rows {
			normalizedJSON := ""
			if results[i].normalized.Symbol != "" {
				b, _ := canonicalJSON(results[i].normalized)
				normalizedJSON = string(b)
			}
			rowDigest := rows[i].RowDigest
			if results[i].normalized.Symbol != "" {
				rowDigest = importIdempotencyDigest(results[i].normalized)
			}
			updates := map[string]any{"status": results[i].status, "error_code": results[i].code, "message": truncateRunes(results[i].message, 250), "normalized_json": normalizedJSON, "row_digest": rowDigest}
			if err := tx.Model(&model.ImportRow{}).Where("id = ? AND user_id = ?", rows[i].ID, userID).Updates(updates).Error; err != nil {
				return err
			}
			switch results[i].status {
			case model.ImportRowValid:
				valid++
			case model.ImportRowConflict:
				conflicts++
			default:
				invalid++
			}
		}
		return tx.Model(&model.ImportBatch{}).Where("id = ? AND user_id = ? AND version = ?", batch.ID, userID, in.Version).Updates(map[string]any{
			"mapping_json": string(mappingJSON), "mapping_digest": mappingDigest, "target_group_id": in.TargetGroupID,
			"status": model.ImportStatusPreviewed, "version": in.Version + 1, "valid_rows": valid, "error_rows": invalid, "conflict_rows": conflicts,
		}).Error
	})
	if err != nil {
		return nil, err
	}
	return s.Get(userID, batch.ID)
}

func (s *DataImportService) normalizePreviewRow(userID, accountID int64, kind string, groupID int64, cells []string, indexes map[string]int, states map[string]*importPreviewState) (importNormalized, string, string) {
	symbol, market, err := validateImportSymbol(importCell(cells, indexes, "symbol"), importCell(cells, indexes, "market"))
	if err != nil {
		return importNormalized{}, "invalid_symbol", err.Error()
	}
	n := importNormalized{Symbol: symbol, Market: market, Name: truncateRunes(importCell(cells, indexes, "name"), 60)}
	switch kind {
	case model.ImportKindWatchlist:
		n.Note = truncateRunes(importCell(cells, indexes, "note"), 500)
		n.FocusReason = truncateRunes(importCell(cells, indexes, "focus_reason"), 500)
		var count int64
		if err := common.DB.Model(&model.WatchlistItem{}).Where("user_id = ? AND watchlist_id = ? AND symbol = ? AND market = ?", userID, groupID, symbol, market).Count(&count).Error; err != nil {
			return n, "database_error", "检查现有自选失败"
		}
		if count > 0 {
			return n, "already_exists", "该股票已在目标分组中"
		}
		return n, "", ""
	case model.ImportKindPosition:
		n.PositionType, err = importPositionType(importCell(cells, indexes, "position_type"))
		if err != nil {
			return n, "invalid_position_type", err.Error()
		}
		n.Price, err = parseImportNumber(importCell(cells, indexes, "price"), "买入价格", true)
		if err != nil {
			return n, "invalid_price", err.Error()
		}
		n.Quantity, err = parseImportNumber(importCell(cells, indexes, "quantity"), "数量", true)
		if err != nil {
			return n, "invalid_quantity", err.Error()
		}
		n.TradeDate, err = parseImportDate(importCell(cells, indexes, "trade_date"))
		if err != nil {
			return n, "invalid_date", err.Error()
		}
		n.Fee, err = parseImportNumber(importCell(cells, indexes, "fee"), "手续费", false)
		if err != nil {
			return n, "invalid_fee", err.Error()
		}
		n.Tax, err = parseImportNumber(importCell(cells, indexes, "tax"), "税费", false)
		if err != nil {
			return n, "invalid_tax", err.Error()
		}
		n.Note = truncateRunes(importCell(cells, indexes, "note"), 500)
		var count int64
		if err := common.DB.Model(&model.Position{}).Where("user_id = ? AND account_id = ? AND symbol = ? AND market = ? AND status = ? AND buy_date = ? AND buy_price = ? AND quantity = ?", userID, accountID, symbol, market, model.PositionStatusHolding, n.TradeDate, n.Price, n.Quantity).Count(&count).Error; err != nil {
			return n, "database_error", "检查现有持仓失败"
		}
		if count > 0 {
			return n, "possible_duplicate", "存在相同标的、日期、价格和数量的持仓，请核对是否重复"
		}
		return n, "", ""
	case model.ImportKindTrade:
		return s.normalizeTradePreview(userID, accountID, cells, indexes, n, states)
	}
	return n, "unsupported_kind", "不支持的导入类型"
}

func (s *DataImportService) normalizeTradePreview(userID, accountID int64, cells []string, indexes map[string]int, n importNormalized, states map[string]*importPreviewState) (importNormalized, string, string) {
	var err error
	n.Side, err = parseImportSide(importCell(cells, indexes, "side"))
	if err != nil {
		return n, "missing_side", err.Error()
	}
	n.Quantity, err = parseImportNumber(importCell(cells, indexes, "quantity"), "成交数量", true)
	if err != nil {
		return n, "invalid_quantity", err.Error()
	}
	n.Price, err = parseImportNumber(importCell(cells, indexes, "price"), "成交价格", true)
	if err != nil {
		return n, "invalid_price", err.Error()
	}
	n.TradeDate, err = parseImportDate(importCell(cells, indexes, "trade_date"))
	if err != nil {
		return n, "invalid_date", err.Error()
	}
	n.Fee, err = parseImportNumber(importCell(cells, indexes, "fee"), "手续费", false)
	if err != nil {
		return n, "invalid_fee", err.Error()
	}
	n.Tax, err = parseImportNumber(importCell(cells, indexes, "tax"), "税费", false)
	if err != nil {
		return n, "invalid_tax", err.Error()
	}
	n.Note = truncateRunes(importCell(cells, indexes, "note"), 200)

	positionIDRaw := importCell(cells, indexes, "position_id")
	key := n.Market + ":" + n.Symbol
	var state *importPreviewState
	if positionIDRaw != "" {
		id, parseErr := strconv.ParseInt(positionIDRaw, 10, 64)
		if parseErr != nil || id <= 0 {
			return n, "invalid_position_id", "持仓 ID 必须为正整数"
		}
		key = "id:" + strconv.FormatInt(id, 10)
		state = states[key]
		if state == nil {
			var p model.Position
			if err := common.DB.Where("id = ? AND user_id = ? AND account_id = ?", id, userID, accountID).First(&p).Error; err != nil {
				return n, "position_not_found", "指定持仓不存在"
			}
			if p.Status != model.PositionStatusHolding {
				return n, "position_closed", "指定持仓已平仓"
			}
			if p.Symbol != n.Symbol || p.Market != n.Market {
				return n, "position_mismatch", "指定持仓与股票代码或市场不一致"
			}
			_, dep, err := positionDependency(common.DB, p)
			if err != nil {
				return n, "database_error", "检查持仓账本失败"
			}
			var last model.PositionTrade
			_ = common.DB.Where("user_id = ? AND account_id = ? AND position_id = ?", userID, accountID, p.ID).Order("trade_date DESC, id DESC").First(&last).Error
			state = &importPreviewState{Position: p, Ledger: ledgerFromPosition(&p), LastTradeDate: last.TradeDate, DependencyHash: dep}
			states[key] = state
			// 只有该标的恰好一笔在持仓位时，才允许后续未填 position_id 的行复用它。
			// 多笔持仓下不能让前一行显式指定的 ID 替后续行消除歧义。
			var holdingCount int64
			if err := common.DB.Model(&model.Position{}).
				Where("user_id = ? AND account_id = ? AND market = ? AND symbol = ? AND status = ?", userID, accountID, p.Market, p.Symbol, model.PositionStatusHolding).
				Count(&holdingCount).Error; err != nil {
				return n, "database_error", "检查同标的持仓失败"
			}
			if holdingCount == 1 {
				states[p.Market+":"+p.Symbol] = state
			}
		}
		if state.ClosedInBatch {
			return n, "ledger_conflict", "指定持仓已被本批前序流水全部卖出，不能用同一持仓 ID 重新买入"
		}
	} else {
		state = states[key]
		if state != nil && state.ClosedInBatch {
			if n.Side != model.PositionTradeBuy {
				return n, "position_not_found", "本批前序流水已全部卖出，后续卖出找不到可用持仓"
			}
			virtual := "new:" + key + ":" + importIdempotencyDigest(n)[:16]
			state = &importPreviewState{Position: model.Position{UserID: userID, AccountID: accountID, Symbol: n.Symbol, Market: n.Market, Name: n.Name, PositionType: model.PositionTypeLongTerm, Status: model.PositionStatusHolding, Currency: defaultCurrencyFor(n.Market)}, VirtualKey: virtual}
			states[key] = state
		}
		if state == nil {
			var positions []model.Position
			if err := common.DB.Where("user_id = ? AND account_id = ? AND symbol = ? AND market = ? AND status = ?", userID, accountID, n.Symbol, n.Market, model.PositionStatusHolding).Order("id ASC").Find(&positions).Error; err != nil {
				return n, "database_error", "检查现有持仓失败"
			}
			if len(positions) > 1 {
				return n, "ambiguous_position", "同一标的有多笔持仓，请在模板中填写持仓 ID"
			}
			if len(positions) == 1 {
				p := positions[0]
				_, dep, err := positionDependency(common.DB, p)
				if err != nil {
					return n, "database_error", "检查持仓账本失败"
				}
				var last model.PositionTrade
				_ = common.DB.Where("user_id = ? AND account_id = ? AND position_id = ?", userID, accountID, p.ID).Order("trade_date DESC, id DESC").First(&last).Error
				state = &importPreviewState{Position: p, Ledger: ledgerFromPosition(&p), LastTradeDate: last.TradeDate, DependencyHash: dep}
				states["id:"+strconv.FormatInt(p.ID, 10)] = state
			} else {
				if n.Side != model.PositionTradeBuy {
					return n, "position_not_found", "卖出流水找不到唯一的在持仓位，不能猜测目标"
				}
				virtual := "new:" + key
				state = &importPreviewState{Position: model.Position{UserID: userID, AccountID: accountID, Symbol: n.Symbol, Market: n.Market, Name: n.Name, PositionType: model.PositionTypeLongTerm, Status: model.PositionStatusHolding, Currency: defaultCurrencyFor(n.Market)}, VirtualKey: virtual}
			}
			states[key] = state
		}
	}
	if state.Position.ID > 0 {
		n.PositionID, n.DependencyHash = state.Position.ID, state.DependencyHash
	} else {
		n.VirtualKey = state.VirtualKey
	}
	if state.LastTradeDate != "" && n.TradeDate < state.LastTradeDate {
		return n, "out_of_order", fmt.Sprintf("成交日期不能早于最近一笔流水日期 %s", state.LastTradeDate)
	}
	if state.Position.BuyDate != "" && n.TradeDate < state.Position.BuyDate {
		return n, "out_of_order", "成交日期不能早于建仓日期"
	}
	if n.Side == model.PositionTradeBuy {
		state.Ledger, err = ledgerBuy(state.Ledger, n.Price, n.Quantity, n.Fee, n.Tax)
	} else {
		state.Ledger, _, err = ledgerSell(state.Ledger, n.Price, n.Quantity, n.Fee, n.Tax)
	}
	if err != nil {
		return n, "ledger_conflict", err.Error()
	}
	state.LastTradeDate = n.TradeDate
	state.ClosedInBatch = state.Ledger.Quantity <= positionQtyEps
	if state.Position.BuyDate == "" {
		state.Position.BuyDate = n.TradeDate
	}
	return n, "", ""
}

func (s *DataImportService) Confirm(ctx context.Context, userID int64, batchID string, in ImportConfirmInput) (*ImportBatchView, error) {
	dataImportMutationMu.Lock()
	defer dataImportMutationMu.Unlock()
	if in.Version <= 0 {
		return nil, errors.New("缺少有效的批次版本")
	}
	err := common.DB.Transaction(func(tx *gorm.DB) error {
		var batch model.ImportBatch
		q := tx.Where("id = ? AND user_id = ?", batchID, userID)
		if !common.UsingSQLite {
			q = q.Clauses(clause.Locking{Strength: "UPDATE"})
		}
		if err := q.First(&batch).Error; err != nil {
			return errors.New("导入批次不存在")
		}
		if batch.Kind == model.ImportKindPosition || batch.Kind == model.ImportKindTrade {
			var account model.PortfolioAccount
			if err := tx.Where("id = ? AND user_id = ? AND status = ?", batch.AccountID, userID, model.PortfolioStatusActive).First(&account).Error; err != nil {
				return errors.New("组合不存在或已归档")
			}
		}
		if batch.Status == model.ImportStatusConfirmed {
			return nil
		}
		if batch.Status == model.ImportStatusRolledBack {
			return errors.New("已回滚批次不能再次确认")
		}
		if batch.Status != model.ImportStatusPreviewed || batch.Version != in.Version {
			return errors.New("预检事实已变化，请刷新后确认")
		}
		if batch.ErrorRows > 0 || batch.ConflictRows > 0 || batch.ValidRows != batch.TotalRows {
			return errors.New("仍有错误或冲突行，修正并重新预检后才能确认")
		}
		var rows []model.ImportRow
		if err := tx.Where("batch_id = ? AND user_id = ? AND status = ?", batch.ID, userID, model.ImportRowValid).Order("row_number ASC").Find(&rows).Error; err != nil {
			return err
		}
		for _, row := range rows {
			var exists int64
			if err := tx.Model(&model.ImportRowClaim{}).Where("user_id = ? AND account_id = ? AND kind = ? AND row_digest = ?", userID, batch.AccountID, batch.Kind, row.RowDigest).Count(&exists).Error; err != nil {
				return err
			}
			if exists > 0 {
				return fmt.Errorf("第 %d 行已被其他批次确认，请重新预检", row.RowNumber)
			}
			claim := model.ImportRowClaim{UserID: userID, AccountID: batch.AccountID, Kind: batch.Kind, RowDigest: row.RowDigest, BatchID: batch.ID, RowNumber: row.RowNumber}
			if err := tx.Create(&claim).Error; err != nil {
				return errors.New("检测到并发重复确认，请刷新批次")
			}
		}
		created, updated, err := s.applyConfirmedRows(ctx, tx, batch, rows)
		if err != nil {
			return err
		}
		now := time.Now()
		if err := tx.Model(&model.ImportBatch{}).Where("id = ? AND user_id = ? AND version = ?", batch.ID, userID, in.Version).Updates(map[string]any{
			"status": model.ImportStatusConfirmed, "version": in.Version + 1, "created_rows": created, "updated_rows": updated, "confirmed_at": &now,
		}).Error; err != nil {
			return err
		}
		return setOnboardingStepTx(tx, userID, OnboardingStepPortfolio, model.OnboardingStepCompleted, 0)
	})
	if err != nil {
		return nil, err
	}
	return s.Get(userID, batchID)
}

func decodeNormalized(row model.ImportRow) (importNormalized, error) {
	var n importNormalized
	if row.NormalizedJSON == "" || json.Unmarshal([]byte(row.NormalizedJSON), &n) != nil {
		return n, errors.New("冻结预检事实损坏")
	}
	return n, nil
}

func (s *DataImportService) applyConfirmedRows(ctx context.Context, tx *gorm.DB, batch model.ImportBatch, rows []model.ImportRow) (int, int, error) {
	switch batch.Kind {
	case model.ImportKindWatchlist:
		return s.confirmWatchlist(tx, batch, rows)
	case model.ImportKindPosition:
		return s.confirmPositions(tx, batch, rows)
	case model.ImportKindTrade:
		return s.confirmTrades(tx, batch, rows)
	default:
		return 0, 0, errors.New("不支持的导入类型")
	}
}

func createImportEffect(tx *gorm.DB, effect model.ImportEffect) error {
	return tx.Create(&effect).Error
}

func (s *DataImportService) confirmWatchlist(tx *gorm.DB, batch model.ImportBatch, rows []model.ImportRow) (int, int, error) {
	var group model.Watchlist
	if err := tx.Where("id = ? AND user_id = ?", batch.TargetGroupID, batch.UserID).First(&group).Error; err != nil {
		return 0, 0, errors.New("目标自选分组已不存在")
	}
	for _, row := range rows {
		n, err := decodeNormalized(row)
		if err != nil {
			return 0, 0, err
		}
		var count int64
		if err := tx.Model(&model.WatchlistItem{}).Where("user_id = ? AND watchlist_id = ? AND symbol = ? AND market = ?", batch.UserID, group.ID, n.Symbol, n.Market).Count(&count).Error; err != nil {
			return 0, 0, err
		}
		if count > 0 {
			return 0, 0, fmt.Errorf("第 %d 行在确认前已加入目标分组，请重新预检", row.RowNumber)
		}
		item := model.WatchlistItem{UserID: batch.UserID, WatchlistID: group.ID, Symbol: n.Symbol, Market: n.Market, Name: orSymbol(n.Name, n.Symbol), Note: n.Note, FocusReason: n.FocusReason}
		if err := tx.Create(&item).Error; err != nil {
			return 0, 0, err
		}
		if err := createImportEffect(tx, model.ImportEffect{UserID: batch.UserID, AccountID: batch.AccountID, BatchID: batch.ID, RowNumber: row.RowNumber, RecordKind: "watchlist_item", RecordID: item.ID, ParentID: group.ID, Action: "create", AfterHash: watchlistImportFingerprint(item)}); err != nil {
			return 0, 0, err
		}
	}
	return len(rows), 0, nil
}

func newImportedPosition(n importNormalized, userID, accountID int64) model.Position {
	p := model.Position{UserID: userID, AccountID: accountID, Symbol: n.Symbol, Market: n.Market, Name: orSymbol(n.Name, n.Symbol), PositionType: n.PositionType, Status: model.PositionStatusHolding, Currency: defaultCurrencyFor(n.Market), BuyPrice: n.Price, BuyDate: n.TradeDate, Quantity: n.Quantity, BuyFee: n.Fee, BuyTax: n.Tax, BuyReason: n.Note}
	if p.PositionType == "" {
		p.PositionType = model.PositionTypeLongTerm
	}
	p.TotalBuyCost = round4(n.Price*n.Quantity + n.Fee + n.Tax)
	p.TotalBuyQty = n.Quantity
	p.RemainingCost = p.TotalBuyCost
	p.PeakPrice, p.PeakFrom = peakInitFor(n.Price, n.TradeDate, time.Now().In(time.Local).Format("2006-01-02"))
	p.PeakDate = p.PeakFrom
	return p
}

func (s *DataImportService) confirmPositions(tx *gorm.DB, batch model.ImportBatch, rows []model.ImportRow) (int, int, error) {
	for _, row := range rows {
		n, err := decodeNormalized(row)
		if err != nil {
			return 0, 0, err
		}
		var count int64
		if err := tx.Model(&model.Position{}).Where("user_id = ? AND account_id = ? AND symbol = ? AND market = ? AND status = ? AND buy_date = ? AND buy_price = ? AND quantity = ?", batch.UserID, batch.AccountID, n.Symbol, n.Market, model.PositionStatusHolding, n.TradeDate, n.Price, n.Quantity).Count(&count).Error; err != nil {
			return 0, 0, err
		}
		if count > 0 {
			return 0, 0, fmt.Errorf("第 %d 行在确认前出现相同持仓，请重新预检", row.RowNumber)
		}
		p := newImportedPosition(n, batch.UserID, batch.AccountID)
		if _, err := fillPositionPeakFromLocalBars(tx, &p, time.Now().In(time.Local).Format("2006-01-02")); err != nil {
			return 0, 0, err
		}
		if err := tx.Create(&p).Error; err != nil {
			return 0, 0, err
		}
		trade := buildInitialTrade(&p)
		trade.Note = "统一导入建仓"
		if err := tx.Create(&trade).Error; err != nil {
			return 0, 0, err
		}
		if err := createImportEffect(tx, model.ImportEffect{UserID: batch.UserID, AccountID: batch.AccountID, BatchID: batch.ID, RowNumber: row.RowNumber, RecordKind: "position_trade", RecordID: trade.ID, ParentID: p.ID, Action: "create", AfterHash: hashTradeForImport(trade)}); err != nil {
			return 0, 0, err
		}
		if err := createImportEffect(tx, model.ImportEffect{UserID: batch.UserID, AccountID: batch.AccountID, BatchID: batch.ID, RowNumber: row.RowNumber, RecordKind: "position", RecordID: p.ID, Action: "create", AfterHash: positionImportFingerprint(p)}); err != nil {
			return 0, 0, err
		}
	}
	return len(rows), 0, nil
}

func hashTradeForImport(t model.PositionTrade) string {
	v := struct {
		ID, UserID, PositionID                   int64
		Side                                     string
		Price, Quantity, Fee, Tax                float64
		TradeDate, Note                          string
		RealizedPnl, AvgCostAfter, QuantityAfter float64
	}{t.ID, t.UserID, t.PositionID, t.Side, t.Price, t.Quantity, t.Fee, t.Tax, t.TradeDate, t.Note, t.RealizedPnl, t.AvgCostAfter, t.QuantityAfter}
	b, _ := json.Marshal(v)
	return hashBytes(b)
}

func (s *DataImportService) confirmTrades(tx *gorm.DB, batch model.ImportBatch, rows []model.ImportRow) (int, int, error) {
	virtualIDs := map[string]int64{}
	checked := map[int64]bool{}
	before := map[int64]importPositionSnapshot{}
	firstRow := map[int64]int{}
	createdPositions := map[int64]bool{}
	recordedTrades := map[int64]bool{}
	createdCount, updatedCount := 0, 0
	for _, row := range rows {
		n, err := decodeNormalized(row)
		if err != nil {
			return 0, 0, err
		}
		positionID := n.PositionID
		if n.VirtualKey != "" {
			positionID = virtualIDs[n.VirtualKey]
			if positionID == 0 {
				if n.Side != model.PositionTradeBuy {
					return 0, 0, fmt.Errorf("第 %d 行不能用卖出创建新持仓", row.RowNumber)
				}
				p := newImportedPosition(n, batch.UserID, batch.AccountID)
				if _, err := fillPositionPeakFromLocalBars(tx, &p, time.Now().In(time.Local).Format("2006-01-02")); err != nil {
					return 0, 0, err
				}
				if err := tx.Create(&p).Error; err != nil {
					return 0, 0, err
				}
				trade := buildInitialTrade(&p)
				trade.Note = truncateRunes(orDefaultStr(n.Note, "统一导入建仓"), 200)
				if err := tx.Create(&trade).Error; err != nil {
					return 0, 0, err
				}
				if err := createImportEffect(tx, model.ImportEffect{UserID: batch.UserID, AccountID: batch.AccountID, BatchID: batch.ID, RowNumber: row.RowNumber, RecordKind: "position_trade", RecordID: trade.ID, ParentID: p.ID, Action: "create", AfterHash: hashTradeForImport(trade)}); err != nil {
					return 0, 0, err
				}
				recordedTrades[trade.ID] = true
				virtualIDs[n.VirtualKey], positionID = p.ID, p.ID
				createdPositions[p.ID] = true
				firstRow[p.ID] = row.RowNumber
				createdCount++
				continue
			}
		}
		var p model.Position
		if err := lockedPosition(tx, batch.UserID, positionID, &p); err != nil {
			return 0, 0, fmt.Errorf("第 %d 行目标持仓已不存在", row.RowNumber)
		}
		if p.AccountID != batch.AccountID {
			return 0, 0, fmt.Errorf("第 %d 行目标持仓已不存在", row.RowNumber)
		}
		if !createdPositions[p.ID] && !checked[p.ID] {
			snap, dep, err := positionDependency(tx, p)
			if err != nil {
				return 0, 0, err
			}
			if dep != n.DependencyHash {
				return 0, 0, fmt.Errorf("第 %d 行目标持仓在预检后发生变化，请重新预检", row.RowNumber)
			}
			before[p.ID] = snap
			firstRow[p.ID] = row.RowNumber
			checked[p.ID] = true
		}
		trade, err := applyImportedTradeTx(tx, &p, n)
		if err != nil {
			return 0, 0, fmt.Errorf("第 %d 行确认失败：%w", row.RowNumber, err)
		}
		// legacy 持仓可能在本次成交前由 ensurePositionTradesTx 惰性补建等价首笔流水。
		// 它也是本批实际创建的数据，必须纳入效果事实，否则无后续操作也会被误判冲突。
		lastBefore := int64(0)
		if snap, ok := before[p.ID]; ok {
			lastBefore = snap.LastTradeID
		}
		var newTrades []model.PositionTrade
		if err := tx.Where("user_id = ? AND account_id = ? AND position_id = ? AND id > ?", batch.UserID, batch.AccountID, p.ID, lastBefore).Order("id ASC").Find(&newTrades).Error; err != nil {
			return 0, 0, err
		}
		for _, createdTrade := range newTrades {
			if recordedTrades[createdTrade.ID] {
				continue
			}
			if err := createImportEffect(tx, model.ImportEffect{UserID: batch.UserID, AccountID: batch.AccountID, BatchID: batch.ID, RowNumber: row.RowNumber, RecordKind: "position_trade", RecordID: createdTrade.ID, ParentID: p.ID, Action: "create", AfterHash: hashTradeForImport(createdTrade)}); err != nil {
				return 0, 0, err
			}
			recordedTrades[createdTrade.ID] = true
		}
		_ = trade
		updatedCount++
	}
	ids := make([]int64, 0, len(firstRow))
	for id := range firstRow {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	for _, id := range ids {
		var p model.Position
		if err := tx.Where("id = ? AND user_id = ? AND account_id = ?", id, batch.UserID, batch.AccountID).First(&p).Error; err != nil {
			return 0, 0, err
		}
		effect := model.ImportEffect{UserID: batch.UserID, AccountID: batch.AccountID, BatchID: batch.ID, RowNumber: firstRow[id], RecordKind: "position", RecordID: id, AfterHash: positionImportFingerprint(p)}
		if createdPositions[id] {
			effect.Action = "create"
		} else {
			effect.Action = "update"
			b, _ := json.Marshal(before[id])
			effect.BeforeJSON = string(b)
		}
		if err := createImportEffect(tx, effect); err != nil {
			return 0, 0, err
		}
	}
	return createdCount, updatedCount, nil
}

func applyImportedTradeTx(tx *gorm.DB, p *model.Position, n importNormalized) (*model.PositionTrade, error) {
	if p.Status != model.PositionStatusHolding {
		return nil, errors.New("持仓已平仓")
	}
	if err := ensurePositionTradesTx(tx, p); err != nil {
		return nil, err
	}
	var last model.PositionTrade
	if err := tx.Where("position_id = ? AND user_id = ? AND account_id = ?", p.ID, p.UserID, p.AccountID).Order("trade_date DESC, id DESC").First(&last).Error; err != nil {
		return nil, err
	}
	if last.TradeDate != "" && n.TradeDate < last.TradeDate {
		return nil, fmt.Errorf("成交日期不能早于最近一笔流水日期 %s", last.TradeDate)
	}
	if n.Side == model.PositionTradeSell {
		if err := ensurePositionShareActionsProcessedTx(tx, *p, n.TradeDate); err != nil {
			return nil, err
		}
	}
	today := time.Now().In(time.Local).Format("2006-01-02")
	if _, err := ensurePositionPeakTx(tx, p, today); err != nil {
		return nil, err
	}
	ledger := ledgerFromPosition(p)
	trade := &model.PositionTrade{UserID: p.UserID, AccountID: p.AccountID, PositionID: p.ID, Side: n.Side, Price: n.Price, Quantity: n.Quantity, Fee: n.Fee, Tax: n.Tax, TradeDate: n.TradeDate, Note: truncateRunes(orDefaultStr(n.Note, "统一导入成交"), 200)}
	var err error
	if n.Side == model.PositionTradeBuy {
		ledger, err = ledgerBuy(ledger, n.Price, n.Quantity, n.Fee, n.Tax)
	} else {
		var realized float64
		ledger, realized, err = ledgerSell(ledger, n.Price, n.Quantity, n.Fee, n.Tax)
		trade.RealizedPnl = realized
	}
	if err != nil {
		return nil, err
	}
	trade.AvgCostAfter, trade.QuantityAfter = ledger.AvgCost, ledger.Quantity
	if err := tx.Create(trade).Error; err != nil {
		return nil, err
	}
	ledger.applyTo(p)
	if n.Side == model.PositionTradeBuy {
		if err := rebuildPositionPeakOnBuyTx(tx, p, n.Price, n.TradeDate, today); err != nil {
			return nil, err
		}
	} else {
		p.SellPrice, p.SellFee, p.SellTax, p.SellDate = n.Price, n.Fee, n.Tax, n.TradeDate
	}
	if ledger.Quantity <= positionQtyEps {
		p.Status = model.PositionStatusClosed
	}
	if err := tx.Save(p).Error; err != nil {
		return nil, err
	}
	if p.Status == model.PositionStatusClosed {
		if err := finalizePositionSellSignalsTx(tx, p.UserID, p.ID, false); err != nil {
			return nil, err
		}
	}
	return trade, nil
}

func (s *DataImportService) Rollback(userID int64, batchID string) (*ImportRollbackResult, error) {
	dataImportMutationMu.Lock()
	defer dataImportMutationMu.Unlock()
	result := &ImportRollbackResult{BatchID: batchID, Conflicts: []ImportRollbackConflict{}}
	// 先做只读冲突检查，逐项返回，不在发现第一个问题时丢失其余冲突。
	var batch model.ImportBatch
	if err := common.DB.Where("id = ? AND user_id = ?", batchID, userID).First(&batch).Error; err != nil {
		return nil, errors.New("导入批次不存在")
	}
	if batch.Status == model.ImportStatusRolledBack {
		result.Status = model.ImportStatusRolledBack
		return result, nil
	}
	if (batch.Kind == model.ImportKindPosition || batch.Kind == model.ImportKindTrade) && batch.AccountID > 0 {
		if _, err := ActivePortfolioAccountByID(userID, batch.AccountID, model.PortfolioKindReal); err != nil {
			return nil, errors.New("组合不存在或已归档")
		}
	}
	if batch.Status != model.ImportStatusConfirmed {
		return nil, errors.New("只有已确认批次可以回滚")
	}
	var effects []model.ImportEffect
	if err := common.DB.Where("batch_id = ? AND user_id = ?", batchID, userID).Order("id ASC").Find(&effects).Error; err != nil {
		return nil, err
	}
	var claims []model.ImportRowClaim
	if err := common.DB.Where("batch_id = ? AND user_id = ?", batchID, userID).Order("row_number ASC").Find(&claims).Error; err != nil {
		return nil, err
	}
	if len(claims) != batch.TotalRows {
		return nil, errors.New("回滚幂等事实不完整，已拒绝自动回滚")
	}
	claimRows := make(map[int]struct{}, len(claims))
	for _, claim := range claims {
		if _, exists := claimRows[claim.RowNumber]; exists {
			return nil, errors.New("回滚幂等事实不完整，已拒绝自动回滚")
		}
		claimRows[claim.RowNumber] = struct{}{}
	}
	effectRows := make(map[int]struct{}, len(effects))
	for _, effect := range effects {
		if _, claimed := claimRows[effect.RowNumber]; !claimed {
			return nil, errors.New("回滚审计事实不完整，已拒绝自动回滚")
		}
		effectRows[effect.RowNumber] = struct{}{}
	}
	for rowNumber := range claimRows {
		if _, exists := effectRows[rowNumber]; !exists {
			return nil, errors.New("回滚审计事实不完整，已拒绝自动回滚")
		}
	}
	var err error
	result.Conflicts, err = s.rollbackConflicts(common.DB, userID, effects, false)
	if err != nil {
		return nil, err
	}
	if len(result.Conflicts) > 0 {
		result.Status = "conflict"
		return result, nil
	}
	err = common.DB.Transaction(func(tx *gorm.DB) error {
		var locked model.ImportBatch
		q := tx.Where("id = ? AND user_id = ?", batchID, userID)
		if !common.UsingSQLite {
			q = q.Clauses(clause.Locking{Strength: "UPDATE"})
		}
		if err := q.First(&locked).Error; err != nil {
			return err
		}
		if locked.Status == model.ImportStatusRolledBack {
			return nil
		}
		if locked.Status != model.ImportStatusConfirmed {
			return errors.New("批次状态已变化")
		}
		conflicts, err := s.rollbackConflicts(tx, userID, effects, true)
		if err != nil {
			return err
		}
		if len(conflicts) > 0 {
			return errors.New("业务数据已变化，请刷新冲突清单")
		}
		if err := s.applyRollback(tx, userID, effects); err != nil {
			return err
		}
		now := time.Now()
		return tx.Model(&model.ImportBatch{}).Where("id = ? AND user_id = ?", batchID, userID).Updates(map[string]any{"status": model.ImportStatusRolledBack, "version": locked.Version + 1, "rolled_back_at": &now}).Error
	})
	if err != nil {
		return nil, err
	}
	result.Status = model.ImportStatusRolledBack
	return result, nil
}

func lockImportRollbackRecords(db *gorm.DB, userID int64, effects []model.ImportEffect) error {
	if common.UsingSQLite {
		return nil
	}
	positionSet, watchlistSet, accountSet := map[int64]bool{}, map[int64]bool{}, map[int64]bool{}
	for _, effect := range effects {
		if effect.AccountID > 0 {
			accountSet[effect.AccountID] = true
		}
		switch effect.RecordKind {
		case "position":
			positionSet[effect.RecordID] = true
		case "position_trade":
			positionSet[effect.ParentID] = true
		case "watchlist_item":
			watchlistSet[effect.RecordID] = true
		}
	}
	positionIDs := make([]int64, 0, len(positionSet))
	for id := range positionSet {
		positionIDs = append(positionIDs, id)
	}
	sort.Slice(positionIDs, func(i, j int) bool { return positionIDs[i] < positionIDs[j] })
	if len(positionIDs) > 0 {
		accountIDs := make([]int64, 0, len(accountSet))
		for id := range accountSet {
			accountIDs = append(accountIDs, id)
		}
		var rows []model.Position
		if err := db.Clauses(clause.Locking{Strength: "UPDATE"}).Where("user_id = ? AND account_id IN ? AND id IN ?", userID, accountIDs, positionIDs).Order("id ASC").Find(&rows).Error; err != nil {
			return err
		}
	}
	watchlistIDs := make([]int64, 0, len(watchlistSet))
	for id := range watchlistSet {
		watchlistIDs = append(watchlistIDs, id)
	}
	sort.Slice(watchlistIDs, func(i, j int) bool { return watchlistIDs[i] < watchlistIDs[j] })
	if len(watchlistIDs) > 0 {
		var rows []model.WatchlistItem
		if err := db.Clauses(clause.Locking{Strength: "UPDATE"}).Where("user_id = ? AND id IN ?", userID, watchlistIDs).Order("id ASC").Find(&rows).Error; err != nil {
			return err
		}
	}
	return nil
}

func (s *DataImportService) rollbackConflicts(db *gorm.DB, userID int64, effects []model.ImportEffect, lock bool) ([]ImportRollbackConflict, error) {
	conflicts := []ImportRollbackConflict{}
	if lock {
		if err := lockImportRollbackRecords(db, userID, effects); err != nil {
			return nil, err
		}
	}
	importedTrades := map[int64]map[int64]bool{}
	for _, e := range effects {
		if e.RecordKind == "position_trade" {
			if importedTrades[e.ParentID] == nil {
				importedTrades[e.ParentID] = map[int64]bool{}
			}
			importedTrades[e.ParentID][e.RecordID] = true
		}
	}
	for _, e := range effects {
		switch e.RecordKind {
		case "position_trade":
			var trade model.PositionTrade
			if err := db.Where("id = ? AND user_id = ? AND account_id = ? AND position_id = ?", e.RecordID, userID, e.AccountID, e.ParentID).First(&trade).Error; err != nil {
				if !errors.Is(err, gorm.ErrRecordNotFound) {
					return nil, err
				}
				conflicts = append(conflicts, ImportRollbackConflict{e.RecordKind, e.RecordID, "本批创建的成交流水已不存在"})
			} else if hashTradeForImport(trade) != e.AfterHash {
				conflicts = append(conflicts, ImportRollbackConflict{e.RecordKind, e.RecordID, "本批创建的成交流水已被修改"})
			}
		case "watchlist_item":
			var item model.WatchlistItem
			if err := db.Where("id = ? AND user_id = ?", e.RecordID, userID).First(&item).Error; err != nil {
				if !errors.Is(err, gorm.ErrRecordNotFound) {
					return nil, err
				}
				conflicts = append(conflicts, ImportRollbackConflict{e.RecordKind, e.RecordID, "本批创建的自选项已不存在"})
			} else if watchlistImportFingerprint(item) != e.AfterHash {
				conflicts = append(conflicts, ImportRollbackConflict{e.RecordKind, e.RecordID, "自选项已被人工编辑"})
			}
		case "position":
			var p model.Position
			if err := db.Where("id = ? AND user_id = ? AND account_id = ?", e.RecordID, userID, e.AccountID).First(&p).Error; err != nil {
				if !errors.Is(err, gorm.ErrRecordNotFound) {
					return nil, err
				}
				conflicts = append(conflicts, ImportRollbackConflict{e.RecordKind, e.RecordID, "本批影响的持仓已不存在"})
				continue
			}
			if positionImportFingerprint(p) != e.AfterHash {
				conflicts = append(conflicts, ImportRollbackConflict{e.RecordKind, e.RecordID, "持仓已发生后续交易、人工编辑或账本调整"})
				continue
			}
			var trades []model.PositionTrade
			if err := db.Where("position_id = ? AND user_id = ? AND account_id = ?", p.ID, userID, e.AccountID).Find(&trades).Error; err != nil {
				return nil, err
			}
			if e.Action == "create" {
				for _, t := range trades {
					if !importedTrades[p.ID][t.ID] {
						conflicts = append(conflicts, ImportRollbackConflict{e.RecordKind, e.RecordID, "新持仓存在非本批创建的后续流水"})
						break
					}
				}
			} else {
				var snap importPositionSnapshot
				if json.Unmarshal([]byte(e.BeforeJSON), &snap) != nil {
					conflicts = append(conflicts, ImportRollbackConflict{e.RecordKind, e.RecordID, "回滚快照损坏"})
					continue
				}
				for _, t := range trades {
					if t.ID > snap.LastTradeID && !importedTrades[p.ID][t.ID] {
						conflicts = append(conflicts, ImportRollbackConflict{e.RecordKind, e.RecordID, "持仓存在本批之后新增的流水"})
						break
					}
				}
			}
			for _, dep := range []struct {
				kind  string
				table any
			}{{"alert_event", &model.AlertEvent{}}, {"position_adjust", &model.PositionCorpAdjust{}}, {"sell_review", &model.SellReview{}}} {
				var count int64
				if err := db.Model(dep.table).Where("user_id = ? AND position_id = ?", userID, p.ID).Count(&count).Error; err != nil {
					return nil, err
				} else if count > 0 {
					conflicts = append(conflicts, ImportRollbackConflict{dep.kind, p.ID, "持仓已产生后续提醒、公司行动或复核依赖"})
					break
				}
			}
			var labelCount int64
			if err := db.Model(&model.RecommendationLabel{}).Where("position_id = ?", p.ID).Count(&labelCount).Error; err != nil {
				return nil, err
			} else if labelCount > 0 {
				conflicts = append(conflicts, ImportRollbackConflict{"recommendation_label", p.ID, "持仓已被推荐回验事实引用"})
			}
		}
	}
	return conflicts, nil
}

func (s *DataImportService) applyRollback(tx *gorm.DB, userID int64, effects []model.ImportEffect) error {
	for _, e := range effects {
		if e.RecordKind == "position_trade" {
			res := tx.Where("id = ? AND user_id = ? AND account_id = ? AND position_id = ?", e.RecordID, userID, e.AccountID, e.ParentID).Delete(&model.PositionTrade{})
			if res.Error != nil {
				return res.Error
			}
			if res.RowsAffected != 1 {
				return errors.New("回滚期间成交流水发生变化")
			}
		}
	}
	for _, e := range effects {
		switch e.RecordKind {
		case "watchlist_item":
			res := tx.Where("id = ? AND user_id = ?", e.RecordID, userID).Delete(&model.WatchlistItem{})
			if res.Error != nil {
				return res.Error
			}
			if res.RowsAffected != 1 {
				return errors.New("回滚期间自选项发生变化")
			}
		case "position":
			if e.Action == "create" {
				res := tx.Where("id = ? AND user_id = ? AND account_id = ?", e.RecordID, userID, e.AccountID).Delete(&model.Position{})
				if res.Error != nil {
					return res.Error
				}
				if res.RowsAffected != 1 {
					return errors.New("回滚期间持仓发生变化")
				}
				continue
			}
			var snap importPositionSnapshot
			if err := json.Unmarshal([]byte(e.BeforeJSON), &snap); err != nil {
				return errors.New("回滚快照损坏")
			}
			snap.Position.ID = e.RecordID
			snap.Position.UserID = userID
			snap.Position.AccountID = e.AccountID
			if err := tx.Save(&snap.Position).Error; err != nil {
				return err
			}
		}
	}
	batchIDs := map[string]bool{}
	for _, e := range effects {
		batchIDs[e.BatchID] = true
	}
	for batchID := range batchIDs {
		if err := tx.Where("user_id = ? AND batch_id = ?", userID, batchID).Delete(&model.ImportRowClaim{}).Error; err != nil {
			return err
		}
	}
	return nil
}

func (s *DataImportService) Template(kind string) ([]byte, string, error) {
	kind, err := normalizeImportKind(kind)
	if err != nil {
		return nil, "", err
	}
	var rows [][]string
	switch kind {
	case model.ImportKindWatchlist:
		rows = [][]string{{"股票代码", "市场", "股票名称", "备注", "关注原因"}, {"600000", "cn", "示例名称", "仅作格式示例", "等待进一步研究"}}
	case model.ImportKindPosition:
		rows = [][]string{{"股票代码", "市场", "股票名称", "持仓类型", "买入价格", "数量", "买入日期", "手续费", "税费", "买入理由"}, {"600000", "cn", "示例名称", "long_term", "10.50", "100", "2026-08-01", "5", "0", "仅作格式示例"}}
	case model.ImportKindTrade:
		rows = [][]string{{"持仓ID", "股票代码", "市场", "股票名称", "买卖方向", "成交数量", "成交价格", "成交日期", "手续费", "税费", "备注"}, {"", "600000", "cn", "示例名称", "buy", "100", "10.50", "2026-08-01", "5", "0", "仅作格式示例"}}
	}
	return writeCSV(rows), fmt.Sprintf("quantvista-%s-import-template.csv", kind), nil
}
