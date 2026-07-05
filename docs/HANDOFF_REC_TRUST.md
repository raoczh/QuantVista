# 交接：推荐流水线重构 + 全 AI 链路信任度增强（2026-07-05）

> 本文档供新会话继续收尾用。**当前仓库状态：后端 build/vet/test 全绿、前端 vue-tsc + vite build 通过（最后一次全量验证于审查修复第 5 项后），可安全继续。**
> 背景与总体设计见 `docs/ROADMAP.md` 顶部「2026-07-04 推荐流水线重构」区块；防回归认知见项目记忆 `quantvista-progress`。

## 一、已完成（不要重做）

**后端**
- 四阶段流水线：`service/recommendation.go` 重写 generate/buildPool/scorePool/buildMessages（prompt p4/strategy s2）；新文件 `service/recfilter.go`（用户筛选：股价/市值/换手/涨停/追高，透明标记式排除）、`service/recfactor.go`（技术因子/策略加分/证据核验 verifyEvidence/程序合成置信度 systemConfidence）。
- 多源建池：自选∪涨幅榜∪成交额榜∪**换手率榜**（`datasource/sina.go` 放开 turnoverratio）；`service/market.go` 加 `GetRanking`（60s 缓存）与 `GetDailyBars` 10min 缓存。
- 模型列：`RecommendationBatch` +Title/FiltersJSON/ReviewJSON；`UserPreference` +RecFiltersJSON（`service/user.go` 有 normalizeRecFiltersJSON 校验）。
- AI 复核员（verify 模式二次调用 reviewPicks/applyReviews）、批次标题 composeBatchTitle、日报动态策略 pickDailyStrategy、分析/问答/对比/日报 prompt 信任增强（引用数值/禁先验记忆/quant_score 锚点，analysis p4）。
- 测试：`recfilter_test.go`、`recfactor_test.go` 共 20 个新用例全过；既有测试未破坏。

**前端**
- `api/recommendation.ts`（RecFilters/PoolCandidate/EvidenceCheck/PickReview/emptyRecFilters）、`api/user.ts`（rec_filters_json）。
- `Recommendations.vue`：筛选区（价格/市值快捷档+自定义、换手区间、追高/涨停开关、AI 复核开关、保存为默认）、候选池全景面板、信任徽章行（量化分/一手成本/数据核验/综合置信/复核结论）、历史标题用 batchTitle（后端 title 优先 + 全量静态策略字典兜底——**"value" 标题 bug 的修复**）。
- `Settings.vue`：偏好加「推荐筛选默认」区。

**对抗性审查（20 代理）确认 9 个净缺陷，已修 7 个：**
1. ✅ 空 picks 合法化：parseAndFilterPicks 用 `Picks *[]recPick` 区分「缺字段」（repair）与「显式空数组」（合法拒选，不 repair）；generate() 空 picks 时保存 rejected_json、降级文案区分「宁缺毋滥」与「无有效输出」。
2. ✅ scorePool 日线拉取失败 → 透明排除（不再按中性 50 分混入排名/绕过追高保护）；追高/无日线释放的评分名额从「池满」标的按序补评一轮（poolFullPrefix 识别）。
3. ✅ buildPool 总量护栏 maxPoolIntake=200（防超大自选打爆估值批量请求）。
4. ✅ 池快照截断 marshalPoolSnapshot（poolSnapshotMax=150，优先保留参与排名者，省略数记入 filters_json 的 `pool_omitted`）。
5. ✅ pickReview.Confidence 改 FlexInt（裸 int 会因模型输出 72.5/"80" 使整个复核 JSON 解析失败被静默丢弃）；复核 prompt 补「confidence 给具体数值；0=维持原值」。
6. ✅ applyReviews reject 强制压低置信度 ≤25（复核给 0 或 30 都压；TestApplyReviews 已按新行为更新，含 reject+confidence=0 用例）。
7. ✅ CandidatePool 列 `type:text` → `type:mediumtext`（AutoMigrate 自动 ALTER，SQLite 无影响）。

## 二、剩余待办（按序做，全部有精确位置）

### A. 审查修复剩余 4 项

1. **verifyEvidence 误报修复**（`service/recfactor.go`）：
   - 无小数点整数跳过规则扩为：噪声集合 ∪ 1900~2100 ∪ ≥1e5 ∪ **≤99**（修「池内第 11」「第 2/38」类 rank/池大小误报——prompt 自己示范的引用格式会被标「可能是幻觉」，属信任层自伤）；
   - 签名加变参 `func verifyEvidence(evidence []string, c candidate, extra ...float64)`，把 extra 并入值域；调用点（recommendation.go 信任层回填循环）传入 `picks[i].BuyZoneLow/BuyZoneHigh/TakeProfit/StopLoss` 与 `filters.PriceMin/PriceMax/MaxGain5dPct/TurnoverMin/TurnoverMax/FloatCapMinYi/FloatCapMaxYi`（模型引用自身计划价/用户阈值不再误报 unmatched）；
   - 更新 `recfactor_test.go` TestVerifyEvidence：加「池内第 11」（应跳过）与「止盈 13.5」（extra 传入后应吻合）用例；注意现有「流通市值 156 亿」用例在 ≤99 规则下仍应吻合（156>99 参与核验）。

2. **换手 20% 硬顶一致化**（数学死局修复：turnover_min>20 时必然空池且报错误导）：
   - `service/recfilter.go` sanitizeRecFilters：TurnoverMin/TurnoverMax 钳制上限从 100 改为 `deadTurnoverPct`（20）；
   - `Recommendations.vue` 与 `Settings.vue` 换手输入框 `:max="100"` → `:max="20"`，提示文案注明「>20% 为死亡换手，系统已硬性排除」；
   - `recfilter_test.go` TestSanitizeRecFilters 期望值同步（TurnoverMax 输入 200 → 期望 20 而非 100）。

3. **前端 generate 前确保偏好已加载**（`Recommendations.vue` generate()）：
   开头加 `if (!pref.value) await loadPrefFilters()`（偏好接口慢/失败时避免静默用内置默认还以为是自己存的偏好）。

4. **前端展示 pool_omitted**（`Recommendations.vue`）：
   appliedFilters 同源解析 filters_json 里的 `pool_omitted`（后端已写入），池全景面板 pool-note 追加「另有 N 只被筛掉的标的未展示（容量保护）」。

### B. 补测试（新行为）

- parseAndFilterPicks：`{"picks":[],"rejected":[{"symbol":"000001","reason":"x"}]}` → err==nil、picks 空、rejected 保留；`{"rejected":[]}`（缺 picks 字段）→ 报错。
- marshalPoolSnapshot：>150 条目时非排除者全保留、omitted 计数正确；≤150 时原样。
- （可选）applyReviews reject 压置信度用例补一行断言。

### C. 收尾验证与提交

1. `cd server && GOPROXY=https://goproxy.cn,direct go build ./... && go vet ./... && go test ./...` 全绿。
2. `cd web && npx vue-tsc --noEmit && npm run build`（本机 Node v24 可直接构建；若回退 v16 见记忆 `quantvista-build-run-baseline` 的 crypto 垫片）。
3. **删除仓库根目录 7 个 `.claude-research-*.md` 临时调研文件**（不入库）。
4. **仓库根目录**执行 `git checkout -- server/web/dist/index.html`（构建会改写它，防混入提交；有过在 server/ 子目录跑相对路径失效的前科）。
5. 一个 commit 提交全部（中文 message，参考 ROADMAP 顶部区块措辞，如 `feat(rec): 推荐四阶段流水线（多源建池/用户筛选/量化评分/LLM精选）+ 全AI链路信任层（证据核验/AI复核/透明池/标题修复）`）。
6. ROADMAP.md 顶部「2026-07-04 推荐流水线重构」区块已写好，若本次有行为增改（如换手钳 20）在「遗留」行补一句即可。

### D. 欠人工（浏览器目验，代码完成后提醒用户）

- 推荐页：筛选表单交互（快捷档→自定义切换）、生成一次真实推荐、候选池全景展开、信任徽章 tooltip、AI 复核开关效果；亮/暗主题至少各一套。
- 设置页「推荐筛选默认」保存后再进推荐页应回填。
- 历史列表旧记录标题回退（应显示中文策略名而非 "value"）。

## 三、防回归认知（改这些代码前先读）

- **parseAndFilterPicks 空数组=合法拒选**（指针语义区分缺字段），别改回「空=报错」——p4 prompt 明示「宁缺毋滥可 0 只」，空数组触发 repair 会强迫模型硬凑标的。
- **scorePool 日线失败=透明排除**，不能让无日线标的按中性 50 分参与排名（会挤占 Top12 且绕过追高保护）。
- **池满补位只补一轮**（poolFullPrefix 前缀识别），拉取总量有界；maxScanCandidates=36 / maxLLMCandidates=12 / maxPoolIntake=200 / poolSnapshotMax=150 四个常量各司其职。
- **换手 >20%（deadTurnoverPct）是无条件硬拦**，用户区间必须被钳在其内，否则 UI 允许的配置构成数学死局。
- **LLM 只见 Top12 子集 map**（poolBySymbol），parseAndFilterPicks 以它做反编造校验——池内但非 Top12 的标的同样会被丢弃，这是有意设计。
- 前端 `STRATEGY_NAME` 静态字典是历史标题 bug（"value"）的兜底，别删；新记录走后端固化的 `batch.title`。
- 日报明日推荐与手动推荐完全同链路（GenerateAuto→generate），筛选偏好 rec_filters_json 对两者同时生效。
