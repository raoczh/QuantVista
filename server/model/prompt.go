package model

import (
	"crypto/sha256"
	"encoding/hex"
	"time"

	"quantvista/common"

	"gorm.io/gorm"
)

// PromptTemplate 用户自定义提示词模板：按模块覆盖默认指引，调 prompt 无需重编译。
// 每用户每模块至多一条（唯一约束）；enabled 时在该模块的 LLM 调用中替换默认指引段。
// module 取值：5 个分析模块（market/sector/stock/watchlist/position，替换 moduleGuidance）
// + 4 个扩展模块（M3c）：recommend 推荐角色与纪律 / daily 收盘日报复盘 / qa 个股问答角色 /
// review 分析复核员。列宽 size:16，新增枚举必须取 ≤16 字符的短名。
//
// P0-6：自定义内容一律作为 L3 任务段注入（模块契约/schema 由系统追加不可覆盖，见
// service/prompt.go composeCustomTaskPrompt）；ContentHash/Revision 提供内容级归因——
// 版本串 base-custom.<hash8> 中的 hash8 即 ContentHash 前 8 位，同名版本必对应同一内容。
type PromptTemplate struct {
	ID      int64  `gorm:"primaryKey" json:"id"`
	UserID  int64  `gorm:"index:idx_pt_uniq,unique" json:"user_id"`
	Module  string `gorm:"size:16;index:idx_pt_uniq,unique" json:"module"`
	Content string `gorm:"type:text" json:"content"`
	Enabled bool   `json:"enabled"`

	// P0-6 内容归因：ContentHash=sha256(Content) hex 前 16 位；Revision 从 1 起每次内容
	// 变化递增（只切 enabled 不变）。升级前的旧行两者为零值，读取侧现算 hash 兼容。
	ContentHash string `gorm:"size:16" json:"content_hash"`
	Revision    int    `json:"revision"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// PromptTemplateRevision 模板内容的不可变快照（P0-6）：每次内容变化落一行，
// (template_id, revision) 唯一，行一旦写入不再更新。审计/manifest 里版本串的 hash8
// 可在此表回查当时的模板原文（llm_call_logs 正文是渲染+组装后的形态，此表是模板
// 原始形态）；删除模板不级联删快照——历史调用的归因链不能随模板删除断掉。
type PromptTemplateRevision struct {
	ID          int64  `gorm:"primaryKey" json:"id"`
	TemplateID  int64  `gorm:"index:idx_ptr_uniq,unique" json:"template_id"`
	UserID      int64  `gorm:"index" json:"user_id"`
	Module      string `gorm:"size:16" json:"module"`
	Revision    int    `gorm:"index:idx_ptr_uniq,unique" json:"revision"`
	ContentHash string `gorm:"size:16" json:"content_hash"`
	Content     string `gorm:"type:text" json:"content"`

	CreatedAt time.Time `json:"created_at"`
}

// 扩展提示词模块（M3c）。分析 5 模块沿用 AnalysisModule* 常量。
const (
	PromptModuleRecommend = "recommend" // 推荐：角色与铁律段（recRoleIntro）
	PromptModuleDaily     = "daily"     // 收盘日报：复盘系统提示（dailyReviewSystem）
	PromptModuleQa        = "qa"        // 个股问答：角色段（qaRoleIntro）
	PromptModuleReview    = "review"    // AI 复核：分析复核员系统提示
)

// PromptContentHash 模板内容 hash（sha256 hex 前 16 位）。对原始模板内容（渲染前）计算——
// 渲染后含运行时变量值，同一模板不同标的会得到不同 hash，失去「归因到模板」的意义。
// 放 model 包供 service 与启动迁移共用同一实现（两份实现会造成归因 hash 漂移）。
func PromptContentHash(content string) string {
	if content == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])[:16]
}

// migratePromptTemplateBaselines P0-6 存量模板基线迁移（AutoMigrate 之后执行，幂等）：
// 升级前的 legacy prompt_templates 行 content_hash=""、revision=0，且 prompt_template_revisions
// 无任何快照——首次修改/删除会让升级前原文永久无法回查。本迁移为每个 legacy 行：
//  1. 按现存正文回填 content_hash 与合法 revision（≥1）；
//  2. 为当前正文补建不可变基线快照（按 (template_id, content_hash) 判存，已有则不重复插入）；
//  3. 不改写任何已存在的快照行（历史 revision 不可变）；
//  4. 处理部分迁移状态：行已回填但快照缺失时只补快照；快照已有但行字段为零值时行 revision
//     与命中快照对齐；行 revision 与快照表 MAX 失步时按 MAX+1 递增防撞 (template_id,revision)
//     唯一索引（与 service 层 Upsert 的失步兜底同语义）。
//
// 重复启动零写入（所有判定都以库内当前状态为准）；单行失败只告警不阻断启动，下次启动幂等重试。
func migratePromptTemplateBaselines(db *gorm.DB) error {
	var rows []PromptTemplate
	if err := db.Find(&rows).Error; err != nil {
		return err
	}
	migrated := 0
	for _, row := range rows {
		row := row
		err := db.Transaction(func(tx *gorm.DB) error {
			wantHash := PromptContentHash(row.Content)
			newHash, newRev := row.ContentHash, row.Revision
			rowDirty := false
			if newHash != wantHash {
				// legacy 空 hash 或（手工改库导致的）hash 与正文不符：以正文现算为准。
				newHash = wantHash
				rowDirty = true
			}
			if newRev <= 0 {
				newRev = 1
				rowDirty = true
			}
			// 当前正文是否已有对应快照（按内容 hash 判存，天然幂等）。
			var match PromptTemplateRevision
			matchErr := tx.Where("template_id = ? AND content_hash = ?", row.ID, wantHash).
				Order("revision DESC").First(&match).Error
			switch {
			case matchErr == nil:
				// 已有基线：行 revision 为零值时与命中快照对齐（部分迁移状态修复）。
				if row.Revision <= 0 && match.Revision > 0 {
					newRev = match.Revision
					rowDirty = true
				}
			case matchErr == gorm.ErrRecordNotFound:
				// 缺基线：补建当前正文的不可变快照。revision 取「行 revision 与快照表 MAX」
				// 之上的合法值，防撞唯一索引（快照表已有其他内容的历史行时按 MAX+1）。
				var maxRev int
				if err := tx.Model(&PromptTemplateRevision{}).Where("template_id = ?", row.ID).
					Select("COALESCE(MAX(revision),0)").Scan(&maxRev).Error; err != nil {
					return err
				}
				if maxRev >= newRev {
					newRev = maxRev + 1
					rowDirty = true
				}
				if err := tx.Create(&PromptTemplateRevision{
					TemplateID: row.ID, UserID: row.UserID, Module: row.Module,
					Revision: newRev, ContentHash: wantHash, Content: row.Content,
				}).Error; err != nil {
					return err
				}
			default:
				return matchErr
			}
			if !rowDirty {
				return nil
			}
			migrated++
			return tx.Model(&PromptTemplate{}).Where("id = ?", row.ID).
				Updates(map[string]any{"content_hash": newHash, "revision": newRev}).Error
		})
		if err != nil {
			common.SysWarn("prompt 模板基线迁移失败（template=%d module=%s，下次启动重试）: %v", row.ID, row.Module, err)
		}
	}
	if migrated > 0 {
		common.SysLog("prompt 模板基线迁移完成：回填 %d 行", migrated)
	}
	return nil
}

// MigratePromptTemplateBaselines 导出入口（Migrate 内部调用；测试可显式调用）。
func MigratePromptTemplateBaselines() error {
	if common.DB == nil {
		return nil
	}
	return migratePromptTemplateBaselines(common.DB)
}
