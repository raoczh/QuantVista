# AI 请求超时与日报/推荐生成链路优化

> 状态：**已实施（2026-07-14），欠部署验证**（验收清单见 ROADMAP §5「异步任务化批」条）  
> 原始排查日期：2026-07-14（codex 起草计划，经评审修订后实施）  
> 适用范围：收盘日报、推荐生成、推荐追踪刷新、LLM 通用调用链路

## 0. 评审修订与实施结果（2026-07-14）

原计划（第 1~10 章，保留在下方作为背景与根因分析）方向正确——根因判断（同步整包接口 + 一次请求串行 2~5 次 AI 调用 + 输出无预算）与「异步任务化 + 单次调用瘦身 + 可降级」三层组合方案均被采纳。实施时做了 6 处修订：

1. **P0「先确认 60s 在哪一层」不作为前置阻塞**——上游 60s 绝对超时已由用户确认不可变；且异步化后浏览器只剩轻量轮询请求，反代超时自动出局。降级为顺手落地的观测项：`llm_call_logs` 新增 `first_chunk_ms` 列（流式首个 data 块到达耗时，**≈latency_ms 即上游忽略 stream 整包返回＝假流式网关**），管理端调用页耗时列展示「首块/总耗时」。
2. **P1 不建通用 jobs 表**（过度设计）——直接复用业务表状态：`recommendation_batches`/`daily_reports` 新增 `processing` 状态，生成接口落 processing 行立即返回（统一响应包络，未用 202），后台 `context.Background()`+总 deadline（推荐 6min/日报 8min）执行后回写。幂等防重：15min 内 processing 复用、超时惰性判 failed。页面刷新凭列表里的 processing 记录恢复轮询。
3. **P2 输出预算数值上调**——原计划 600~1500 对中文 JSON 偏紧，截断 JSON 会触发 repair 反而更慢。实施值：复盘 1500 / 推荐主调与 repair 2500 / 复核 1500，取 `min(用户 max_tokens, 模块上限)`（`capModuleTokens`）。真正的瘦身杠杆在 prompt 输出纪律：reason ≤3 条、risks ≤3 条、evidence ≤4 条每条 ≤40 字、落选理由 ≤20 字、复盘各段句数上限（推荐 p11、日报 d2）。Top 16 → **10**。
4. **P4 降级范围收窄**——只对超时/网络/5xx/流中断/空内容类失败做量化降级（`quantFallbackEligible`）；「模型宁缺毋滥拒选」是既有合法语义，不被量化票覆盖；鉴权/路径/配额类确定性错误直接失败让用户修配置。降级条目：ATR 规则计划价（止损 2×ATR%/止盈 3×ATR%/区间 ±0.5×ATR%，钳 [2.5,6]）、恒 watch、置信 35、`degraded_source=quant_fallback` 标记、系统置信度强制 low。
5. **P5 SSE 砍掉**——计划自述「轮询优先，SSE 仅体验增强」，个人自用 2.5s 轮询足够（`web/src/lib/poll.ts` pollUntil）。
6. **流式改造保留并修正**（§4 指出的问题）——保留「流式优先 + 上游拒绝 stream 自动回落非流式」（对空闲超时型网关有效且无害），修正审计不一致：`chatCompletion` 出口按**实际请求形态**记 stream 列（原先固定记 false）；「504/超时/取消不自动重试」经核对现状已满足（`transientNetErr` 排除超时、`retryableStatus` 不含 504），未改。

已落地清单（对应第 7 章实施顺序的第一、二、三阶段合并完成）：

- **ai_client**：审计记实际形态 + `first_chunk_ms`（chat 与 responses 两条流式路径）+ `capModuleTokens`。
- **推荐**：异步任务化（processing 批次 + 后台独立 ctx + 幂等防重 + panic 兜底）、Top16→10、repair 2→1 且坏输出回灌截断 600 字、prompt 输出纪律（p11）、量化降级、模块输出预算。
- **日报**：异步任务化（重生成原地置 processing、内容保留、双败回滚旧状态——替代旧版「先删后生成」）、复盘与推荐两路 goroutine 并行、复盘预算 1500、repair 回灌 2000→800（d2）、自动链路失败落 failed 行防重试烧 token 逻辑保留。
- **追踪刷新**：条目受控并发 4（upsert 收集后串行落库）、degraded 批次纳入追踪（降级推荐有规则价位）、前端独立 60s 超时。
- **前端**：两页生成改「秒回 + 轮询」状态机（processing 横幅/历史「生成中」tag/刷新恢复跟踪/量化降级标签），生成请求撤销 5 分钟超长超时。
- **测试**：推荐异步壳与幂等、量化降级价位手工验算、repair 预算与截断、日报并行（双向进场信号）/重生成保旧/幂等、审计实际形态与假流式识别、capModuleTokens 表驱动；全量 go test 与 vue-tsc 通过。

防回归认知固化在 `docs/ROADMAP.md` §3「AI 请求超时与生成链路（异步任务化批）」条，架构说明在 `docs/ARCHITECTURE.md` §6.10。**部署验证是本批核心收尾**（就是为了修线上 60s 超时），清单见 ROADMAP §5。

---

以下为原始计划（背景与根因分析仍然有效；方案部分以上方修订为准）。

## 1. 背景与目标

当前收盘日报中的“明日推荐”和推荐追踪页面中的“生成推荐”经常在约 1 分钟后失败。此前已尝试把 Go 服务到 AI 上游的请求改为流式，但实际效果不稳定。

本优化的目标是：

1. 浏览器、宝塔/Nginx 或页面刷新不再中断生成任务。
2. 在上游单次请求绝对限制为 60 秒且无法调整的前提下，确保每一次 AI 调用尽量在 55 秒以内完成。
3. 收盘日报的复盘与推荐可以分阶段完成，单个环节失败不阻断其他结果。
4. AI 超时时仍能返回量化推荐或已有结果，不因 AI 文案失败而完全没有推荐个股。
5. 任务状态、失败层级、首个流式数据时间和实际耗时可观测，便于区分 AI 上游超时与网站反向代理超时。

## 2. 当前超时配置

| 链路 | 当前值 | 位置 | 说明 |
| --- | ---: | --- | --- |
| 前端 Axios 默认超时 | 20 秒 | `web/src/api/client.ts` | 普通请求默认值 |
| 收盘日报生成 | 5 分钟 | `web/src/api/report.ts` | 普通 Axios POST，等待完整 JSON |
| 推荐生成 | 5 分钟 | `web/src/api/recommendation.ts` | 普通 Axios POST，等待完整 JSON |
| 推荐刷新追踪 | 20 秒 | `web/src/api/recommendation.ts` | 未单独覆盖超时，沿用全局值 |
| Go 入站请求 | 未显式设置 | `server/main.go` | Gin `engine.Run`，没有 Read/WriteTimeout |
| 普通 AI HTTP 请求 | 90 秒 | `server/service/ai_client.go` | `http.Client.Timeout` |
| 当前流式 AI 请求 | 总超时为 0 | `server/service/ai_client.go` | 响应头超时 90 秒，无调用方 deadline 时兜底 10 分钟 |
| 数据源单能力总预算 | 15 秒 | `server/datasource/manager.go` | 单个数据源预算 6 秒 |
| 自动日报单用户任务 | 8 分钟 | `server/service/dailyreport.go` | 后台任务 deadline |
| 后台推荐追踪单用户任务 | 5 分钟 | `server/service/tracking.go` | 后台任务 deadline |

`deploy/docker-compose.yml` 中的 10 秒 timeout 仅用于健康检查，不是业务请求超时。

仓库中没有宝塔/Nginx 的 `proxy_read_timeout` 等线上反向代理配置，因此源码无法确认线上约 60 秒是否还包含网站反代限制。

## 3. 当前调用链

### 3.1 收盘日报

当前为一个同步 HTTP 请求，所有步骤串行完成后才向浏览器返回结果：

```text
浏览器 POST /daily-reports/generate
  → 构建市场/持仓/自选快照
  → AI 生成复盘
      → JSON 不合格时 repair 1 次
  → 构建推荐候选池和量化评分
  → AI 生成推荐
      → 校验不合格时 repair 最多 2 次
  → 创建卖点提醒
  → 落库日报
  → 一次性返回完整 JSON
```

一份日报最少调用 AI 2 次，最坏调用 AI 5 次。即使每次 AI 调用都小于 60 秒，整个浏览器请求仍很容易超过 1 分钟。

主要代码位置：

- `server/controller/dailyreport.go`
- `server/service/dailyreport.go`
- `server/service/recommendation.go`

### 3.2 推荐生成

```text
浏览器 POST /recommendations
  → 多源构建候选池
  → 本地筛选与量化评分
  → Top 16 进入 LLM
  → AI 精选推荐
      → 校验失败时 repair 最多 2 次
  → 可选 AI 复核
      → 最多再调用 2 次
  → 落库批次和推荐条目
  → 一次性返回完整 JSON
```

普通推荐最少调用 AI 1 次，最坏 3 次；开启 AI 复核后还可能增加 1～2 次。当前前端默认未开启复核。

### 3.3 推荐刷新追踪

推荐刷新追踪不调用 AI，主要逐条执行：

```text
拉取基准日线
  → 拉取每只推荐股票的日线
  → 拉取每只股票的实时行情
  → 计算止盈/止损/收益/回撤/Alpha
  → Upsert 追踪状态
```

该过程目前按推荐条目串行处理，而前端只有 20 秒超时。单项数据源总预算可达到 15 秒，因此多只股票刷新时容易超过前端限制。

## 4. 当前流式改造的实际边界

当前未提交的流式改造实现的是：

```text
Go 服务 ──SSE 流式──> AI 上游
```

但浏览器侧仍然是：

```text
浏览器 ──普通 POST──> Go 服务
                         │
                         └── 等全部业务完成后一次性返回 JSON
```

`chatCompletion` 内部调用 `chatCompletionStreamInner(..., nil)`，AI 增量只在 Go 内存中聚合，没有通过 Controller 发送给浏览器。因此：

1. 无法为浏览器或宝塔/Nginx 提供响应字节和心跳。
2. 浏览器或反代断开后，`c.Request.Context()` 会取消，正在执行的上游 AI 请求也会被取消。
3. 如果上游是单次请求绝对 60 秒限制，流式无法绕过。
4. 如果模型首个 token 超过 60 秒，流式无法绕过。
5. 如果兼容网关忽略 `stream=true` 并整包返回，仍然会触发 60 秒问题。

当前改造还存在以下隐患：

- 所有 `chatCompletion` 被无条件改为优先流式，未遵循 `LLMConfig.Stream` 配置。
- 外层调用审计仍可能记录 `stream=false`，与实际请求形态不一致。
- 流在接近 60 秒时被上游中断，半截内容会整体判失败且不重试。
- 流式只解决空闲超时，不能解决绝对总时长限制。

## 5. 根因判断

当前问题不是单纯把前端超时从 1 分钟改成 5 分钟就能解决，主要由以下因素叠加造成：

1. 日报和推荐仍然是同步整包接口，浏览器长时间收不到任何响应数据。
2. 日报一次请求串行执行 2～5 次 AI 调用。
3. 推荐主调用最多执行 3 次，开启复核后调用次数更多。
4. 推荐提示词较重：Top 16 候选、字段说明很长、每只包含大量因子，并要求为所有未入选候选生成落选理由。
5. `max_tokens` 直接沿用用户全局配置，缺少日报/推荐模块级输出预算。
6. repair 会把上一轮完整错误输出重新加入上下文，使后续请求更慢。
7. 线上可能同时存在 AI 上游 60 秒和宝塔/Nginx 60 秒两种限制。

## 6. 总体优化方案

推荐采用“异步任务化 + 单次 AI 调用瘦身 + 可降级结果”三部分组合方案。仅改流式或仅调大前端超时都不能完整解决问题。

### 6.1 P0：先确认 60 秒发生在哪一层

实施前先收集证据，避免把 AI 上游超时误判为网站反代超时。

需要检查：

1. 管理端 `llm_call_logs` 中 `daily_report`、`recommendation`、`rec_review` 的 latency、status 和 error。
2. 宝塔/Nginx access/error log 中对应请求是否约 60 秒返回 499/502/504。
3. 服务端日志中是否出现 `context canceled`、`deadline exceeded`、上游 HTTP 504 或“流式响应中断”。
4. 对同一模型做直接 `curl -N` 测试，确认是否在 60 秒内持续收到 SSE chunk。
5. 对比后台自动日报和手动日报：后台能成功、手动失败，通常说明浏览器或网站反代链路存在限制。

后续需要补充的观测指标：

- 请求总耗时。
- 首响应头耗时。
- 首 chunk 耗时。
- chunk 数量和最后一个 chunk 时间。
- 实际是否为 SSE。
- 上游状态码和错误分类。
- 输入字符数、估算输入 token、输出 token。

### 6.2 P1：日报和推荐改为异步任务

生成接口不再等待整个任务完成，而是立即创建任务并返回：

```json
{
  "job_id": "xxx",
  "status": "pending"
}
```

建议状态机：

```text
pending
  → collecting
  → reviewing
  → recommending
  → finalizing
  → success / partial / failed
```

前端每 2～3 秒轮询任务状态，也可以使用 SSE 只传任务进度事件，不直接传模型 token。

任务执行必须使用独立于 HTTP 请求的 Context，并设置合理的任务总 deadline，避免浏览器关闭、刷新或反代断开后任务被取消。

建议接口形态：

```text
POST /api/daily-reports/generate
  → 202 Accepted + job_id

GET /api/jobs/:id
  → 当前阶段、进度、错误、report_id/batch_id

POST /api/recommendations
  → 202 Accepted + job_id/batch_id

GET /api/recommendations/:id
  → processing/success/degraded/failed
```

需要具备：

- 同一用户同一类型任务的幂等和防重复提交。
- 处理中再次点击时返回已有 job，而不是重复调用 AI。
- 推荐批次先创建 `processing` 状态，完成后更新。
- 日报重生成时不要在新报告成功前删除旧报告。
- 页面刷新后可以恢复任务进度。
- 前端超时或断线后自动查询最终结果。

异步任务能解决浏览器和网站反代超时，但不能突破 AI 上游单次绝对 60 秒，所以必须同时实施 P2。

### 6.3 P2：确保单次 AI 调用在 55 秒以内

建议给不同模块设置独立输出预算，不直接使用用户全局 `max_tokens`：

| 模块 | 建议压测起点 |
| --- | ---: |
| 日报复盘 | 600～1000 tokens |
| 推荐主调用 | 1000～1500 tokens |
| 推荐 repair | 500～800 tokens |
| AI 复核 | 600～1000 tokens |

具体数值需按实际模型输出速度压测，目标是在最慢可接受场景下仍小于 55 秒。

推荐提示词瘦身：

1. Top 16 候选缩减为 8～10。
2. 根据短线/长线和当前策略，只发送真正参与判断的字段。
3. 删除提示词中重复、可通过简短 schema 表达的字段解释。
4. 数字统一合理精度，避免无意义长小数。
5. 不要求 AI 为所有未入选候选生成 `rejected` 理由。
6. 落选理由优先由本地量化规则合成，或只要求 AI 解释少量重点落选项。
7. 每只推荐限制 reason、risks、evidence 的条数和单条长度。
8. 推荐任务优先使用速度稳定的模型；上游支持时关闭或降低 reasoning effort。
9. 推荐和日报使用较低 temperature，减少 JSON 漂移和 repair 概率。

### 6.4 P3：减少 repair 和串行调用

repair 优化：

1. 优先本地修复常见 JSON 问题，例如代码块包裹、尾逗号和前后解释文字。
2. AI repair 最多 1 次。
3. repair 不再附加上一轮完整坏输出。
4. repair 仅携带错误摘要、允许的 symbol、最小 schema 和必要字段。
5. 上游超时、504、context canceled 不做同请求自动重试，避免再次等待接近 60 秒。

日报并行化：

- 快照完成后，日报复盘和推荐可以作为两个独立子任务并行执行。
- 任一子任务先完成即可先落库并展示。
- 推荐失败时日报保持 `partial`，不影响复盘结果。
- 复盘失败时推荐仍可正常展示。

AI 复核：

- 保持默认关闭。
- 推荐主结果成功后再异步复核。
- 复核失败不影响已生成推荐。

### 6.5 P4：增加可靠降级策略

当前量化系统已经完成候选筛选和排序，AI 的主要作用是精选、解释和生成计划。因此 AI 超时时不应完全没有推荐结果。

建议降级顺序：

1. AI 正常完成：保存完整 AI 推荐和解释。
2. AI 精选超时：使用量化 Top N 生成 `degraded` 推荐。
3. 计划价位可以由 ATR、现价和固定盈亏比等本地规则生成，并明确标记“规则生成，未经 AI 解读”。
4. AI 解读可作为后续异步补全任务。
5. 页面清晰区分“AI 推荐”“量化降级推荐”“AI 解读待补充”。

该方案可以保证上游波动时仍然能够生成候选个股，而不是整个功能不可用。

### 6.6 P5：下游流式仅用于进度和心跳

如果保留长连接，应新增真正贯穿到浏览器的 SSE/NDJSON 接口：

- Controller 立即发送响应头并 `Flush`。
- 每 10～15 秒发送心跳。
- 返回 collecting/reviewing/recommending 等阶段事件。
- 设置 `X-Accel-Buffering: no`。
- Nginx 关闭响应缓冲，并按需要调整 `proxy_read_timeout`。

推荐和日报必须等完整 JSON 校验后才能落库，逐 token 展示的业务价值有限。因此优先选择“异步 job + 轮询”，SSE 仅作为进度体验增强。

### 6.7 P6：推荐追踪刷新单独优化

推荐刷新追踪不涉及 AI，应与 AI 超时问题分开处理：

1. 前端为追踪刷新设置独立超时，不再沿用全局 20 秒。
2. 单批次多只股票使用受控并发，避免逐只完全串行。
3. 优先读取本地日线和 Redis 缓存，只在缺数据时请求上游。
4. 基准日线按市场缓存，避免重复拉取。
5. 数据源失败时保留旧追踪结果并标记本次刷新部分失败。
6. 条目较多时也可改成异步刷新任务。

## 7. 建议实施顺序

### 第一阶段：可观测与快速止损

- 区分 AI 上游 60 秒与宝塔/Nginx 60 秒。
- 增加首 chunk、chunk 数和输入大小记录。
- 修正实际流式状态的审计字段。
- 推荐/日报增加模块级 max token。
- 推荐 Top 16 缩减到 8～10。
- repair 最多 1 次且不回灌完整坏输出。
- AI 复核保持默认关闭并移出主链路。

### 第二阶段：任务化改造

- 新增通用 job 状态模型或为日报/推荐增加 processing 状态。
- 生成接口立即返回 job_id。
- 后台使用独立 Context 执行。
- 前端轮询任务状态并支持页面刷新恢复。
- 增加幂等和重复点击保护。

### 第三阶段：降级与体验

- AI 失败时生成量化降级推荐。
- 日报复盘和推荐分别落库、并行执行。
- 增加阶段进度和错误层级展示。
- 需要时增加 SSE 进度和心跳。
- 推荐追踪刷新改为受控并发或异步。

## 8. 验收标准

### 8.1 收盘日报

- 点击生成后 2 秒内返回任务 ID 或进入 processing 状态。
- 浏览器刷新或关闭页面后，后台任务继续执行。
- 复盘和推荐分别记录状态，任一失败时另一部分仍可展示。
- 单次 AI 请求 P95 小于 55 秒。
- 日报总任务超过 1 分钟时页面不会报网络超时。
- 重复点击不会创建多个相同任务或重复消耗配额。

### 8.2 推荐生成

- 推荐主调用 P95 小于 55 秒。
- 正常情况最多 1 次主调用加 1 次 repair。
- AI 超时时仍能保存量化降级推荐。
- AI 复核不阻塞主结果。
- 页面刷新后能恢复批次状态。

### 8.3 推荐追踪

- 5 只推荐的刷新不再受前端 20 秒限制影响。
- 单只数据源失败不会导致整个批次刷新失败。
- 重复刷新幂等，不产生重复状态记录。

### 8.4 可观测性

- 能明确看到错误发生在浏览器反代、Go 服务、AI 网关还是模型端。
- 每次 AI 调用记录首 chunk 时间、总耗时、输入大小、token、状态码和错误类型。
- 实际请求是否流式与审计字段一致。

## 9. 风险与注意事项

1. 异步任务不能突破 AI 上游绝对 60 秒，必须与提示词和输出瘦身同时实施。
2. 并行执行日报复盘和推荐可能增加同一时刻的模型并发，需要设置用户级或全局并发限制。
3. 量化降级推荐必须明确标识，避免被误认为完整 AI 推荐。
4. 任务 Context 不能直接复用 `c.Request.Context()`，但仍需设置明确总 deadline，防止后台任务永久挂起。
5. 手动重生成日报时应保留旧报告，直到新报告成功或至少完成可用部分。
6. 配额应按实际完成的 AI 调用和 token 记账，重复轮询不能重复计费。
7. 当前流式相关改动仍在未提交工作区中，实施前应先决定保留、收窄或回退其影响范围。

## 10. 涉及文件参考

- `web/src/api/client.ts`
- `web/src/api/report.ts`
- `web/src/api/recommendation.ts`
- `web/src/pages/DailyReport.vue`
- `web/src/pages/Recommendations.vue`
- `server/main.go`
- `server/controller/dailyreport.go`
- `server/controller/recommendation.go`
- `server/service/ai_client.go`
- `server/service/ai_client_responses.go`
- `server/service/dailyreport.go`
- `server/service/recommendation.go`
- `server/service/tracking.go`
- `server/service/llm_call_log.go`
- `server/datasource/manager.go`

