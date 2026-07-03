<script setup lang="ts">
import { computed, onMounted, reactive, ref } from 'vue'
import { useRouter } from 'vue-router'
import {
  NTabs,
  NTabPane,
  NButton,
  NSpace,
  NTable,
  NTag,
  NModal,
  NForm,
  NFormItem,
  NInput,
  NInputNumber,
  NSelect,
  NSwitch,
  NPopconfirm,
  NEmpty,
  useMessage,
} from 'naive-ui'
import {
  listLLMConfigs,
  createLLMConfig,
  updateLLMConfig,
  deleteLLMConfig,
  testLLMConfig,
  testLLMDraft,
  type LLMConfig,
  type LLMConfigInput,
} from '@/api/llm'
import { getPreference, updatePreference, changePassword, getQuota, type UserPreference, type UserQuota, type BlacklistEntry } from '@/api/user'
import { downloadExport, type ExportKind } from '@/api/export'
import { useAuthStore } from '@/stores/auth'
import PageContainer from '@/components/PageContainer.vue'
import SectionCard from '@/components/SectionCard.vue'

const message = useMessage()
const router = useRouter()
const auth = useAuthStore()

/* ---------------- LLM 配置 ---------------- */
const configs = ref<LLMConfig[]>([])
const loadingConfigs = ref(false)
const showModal = ref(false)
const editingId = ref<number | null>(null)
const testing = ref(false)
const saving = ref(false)

const blankForm = (): LLMConfigInput => ({
  name: '',
  provider: 'openai',
  base_url: '',
  api_key: '',
  model: '',
  temperature: 0.7,
  max_tokens: 2048,
  stream: true,
  is_default: false,
})
const form = reactive<LLMConfigInput>(blankForm())

const providerOptions = [
  { label: 'OpenAI 兼容（OpenAI/DeepSeek/Moonshot/中转等）', value: 'openai' },
  { label: '其他', value: 'other' },
]

async function loadConfigs() {
  loadingConfigs.value = true
  try {
    configs.value = await listLLMConfigs()
  } catch (e) {
    message.error((e as Error).message)
  } finally {
    loadingConfigs.value = false
  }
}

function openCreate() {
  editingId.value = null
  Object.assign(form, blankForm())
  showModal.value = true
}

function openEdit(cfg: LLMConfig) {
  editingId.value = cfg.id
  Object.assign(form, {
    name: cfg.name,
    provider: cfg.provider || 'openai',
    base_url: cfg.base_url,
    api_key: '', // 留空表示保留原密钥
    model: cfg.model,
    temperature: cfg.temperature,
    max_tokens: cfg.max_tokens,
    stream: cfg.stream,
    is_default: cfg.is_default,
  })
  showModal.value = true
}

async function save() {
  saving.value = true
  try {
    if (editingId.value) {
      await updateLLMConfig(editingId.value, { ...form })
      message.success('已更新')
    } else {
      await createLLMConfig({ ...form })
      message.success('已创建')
    }
    showModal.value = false
    await loadConfigs()
  } catch (e) {
    message.error((e as Error).message)
  } finally {
    saving.value = false
  }
}

async function remove(cfg: LLMConfig) {
  try {
    await deleteLLMConfig(cfg.id)
    message.success('已删除')
    await loadConfigs()
  } catch (e) {
    message.error((e as Error).message)
  }
}

async function testSaved(cfg: LLMConfig) {
  try {
    const r = await testLLMConfig(cfg.id)
    r.ok ? message.success(`连接成功（${r.latency_ms}ms）`) : message.error(`失败：${r.message}`)
  } catch (e) {
    message.error((e as Error).message)
  }
}

async function testDraft() {
  if (!form.api_key) return message.warning('即时测试需填写 API Key（保存后可对已存配置直接测试）')
  testing.value = true
  try {
    const r = await testLLMDraft({ ...form })
    r.ok ? message.success(`连接成功（${r.latency_ms}ms）`) : message.error(`失败：${r.message}`)
  } catch (e) {
    message.error((e as Error).message)
  } finally {
    testing.value = false
  }
}

/* ---------------- 用户偏好 ---------------- */
const pref = ref<UserPreference | null>(null)
const savingPref = ref(false)
const riskOptions = [
  { label: '保守', value: 'conservative' },
  { label: '均衡', value: 'balanced' },
  { label: '激进', value: 'aggressive' },
]
const marketOptions = [
  { label: 'A 股', value: 'cn' },
]
const horizonOptions = [
  { label: '短线', value: 'short_term' },
  { label: '中线', value: 'mid_term' },
  { label: '长线', value: 'long_term' },
]

async function loadPref() {
  try {
    pref.value = await getPreference()
    parseBlacklist(pref.value.blacklist_json)
  } catch (e) {
    message.error((e as Error).message)
  }
}

/* ---------------- 候选池回避规则（黑名单 + 成交额门槛） ---------------- */
const blacklist = ref<BlacklistEntry[]>([])
const newBlack = reactive({ symbol: '', reason: '' })

function parseBlacklist(raw: string) {
  try {
    const arr = raw ? (JSON.parse(raw) as BlacklistEntry[]) : []
    blacklist.value = Array.isArray(arr) ? arr : []
  } catch {
    blacklist.value = []
  }
}
function addBlack() {
  const sym = newBlack.symbol.trim()
  if (!sym) {
    message.warning('请输入股票代码')
    return
  }
  if (blacklist.value.some((b) => b.symbol === sym)) {
    message.warning('该代码已在黑名单中')
    return
  }
  blacklist.value.push({ symbol: sym, market: 'cn', reason: newBlack.reason.trim() })
  newBlack.symbol = ''
  newBlack.reason = ''
}
function removeBlack(i: number) {
  blacklist.value.splice(i, 1)
}
// 门槛以亿元展示，落库仍为元。
const minAmountYi = computed({
  get: () => (pref.value ? pref.value.min_candidate_amount / 1e8 : 0),
  set: (v: number | null) => {
    if (pref.value) pref.value.min_candidate_amount = Math.round((v || 0) * 1e8)
  },
})

/* ---------------- AI 配额用量 ---------------- */
const quota = ref<UserQuota | null>(null)
async function loadQuota() {
  try {
    quota.value = await getQuota()
  } catch {
    /* 配额展示失败不打扰用户 */
  }
}

async function savePref() {
  if (!pref.value) return
  savingPref.value = true
  try {
    pref.value.blacklist_json = blacklist.value.length ? JSON.stringify(blacklist.value) : ''
    pref.value = await updatePreference(pref.value)
    parseBlacklist(pref.value.blacklist_json)
    message.success('偏好已保存')
  } catch (e) {
    message.error((e as Error).message)
  } finally {
    savingPref.value = false
  }
}

/* ---------------- 账号安全：修改密码 ---------------- */
const pw = reactive({ old: '', next: '', confirm: '' })
const savingPw = ref(false)

async function submitChangePassword() {
  if (pw.next.length < 8) return message.error('新密码至少 8 个字符')
  if (pw.next !== pw.confirm) return message.error('两次输入的新密码不一致')
  savingPw.value = true
  try {
    await changePassword(pw.old, pw.next)
    message.success('密码已修改，请用新密码重新登录')
    pw.old = ''
    pw.next = ''
    pw.confirm = ''
    // 改密后旧 access token 已失效，登出并跳转登录页。
    await auth.logout()
    router.replace('/login')
  } catch (e) {
    message.error((e as Error).message)
  } finally {
    savingPw.value = false
  }
}

onMounted(() => {
  loadConfigs()
  loadPref()
  loadQuota()
})

/* 数据导出（批次 J）：四类数据一键导出 CSV。 */
const exportOptions: { kind: ExportKind; label: string }[] = [
  { kind: 'positions', label: '导出持仓' },
  { kind: 'watchlist', label: '导出自选' },
  { kind: 'recommendations', label: '导出推荐' },
  { kind: 'analyses', label: '导出分析历史' },
]
const exporting = ref<ExportKind | null>(null)
async function doExport(kind: ExportKind) {
  exporting.value = kind
  try {
    await downloadExport(kind)
    message.success('已开始下载')
  } catch (e) {
    message.error((e as Error).message)
  } finally {
    exporting.value = null
  }
}
</script>

<template>
  <PageContainer title="设置" subtitle="模型 · 偏好 · 账号安全">
    <n-tabs type="line" animated>
    <!-- LLM 配置 -->
    <n-tab-pane name="llm" tab="LLM 配置">
      <SectionCard :hoverable="false">
        <div class="card-toolbar">
          <span class="ct-title">已配置的模型服务</span>
          <n-button type="primary" size="small" @click="openCreate">新增配置</n-button>
        </div>
        <n-empty v-if="!loadingConfigs && configs.length === 0" description="还没有 LLM 配置" />
        <n-table v-else :bordered="false" :single-line="false">
          <thead>
            <tr>
              <th>名称</th>
              <th>模型</th>
              <th>Base URL</th>
              <th>密钥</th>
              <th>默认</th>
              <th>操作</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="c in configs" :key="c.id">
              <td>{{ c.name }}</td>
              <td>{{ c.model }}</td>
              <td>{{ c.base_url }}</td>
              <td>
                <n-tag :type="c.has_api_key ? 'success' : 'warning'" size="small" round>
                  {{ c.has_api_key ? '已设置' : '未设置' }}
                </n-tag>
              </td>
              <td>
                <n-tag v-if="c.is_default" type="info" size="small" round>默认</n-tag>
              </td>
              <td>
                <n-space :size="6">
                  <n-button size="tiny" @click="testSaved(c)">测试</n-button>
                  <n-button size="tiny" @click="openEdit(c)">编辑</n-button>
                  <n-popconfirm @positive-click="remove(c)">
                    <template #trigger><n-button size="tiny" type="error">删除</n-button></template>
                    确认删除「{{ c.name }}」？
                  </n-popconfirm>
                </n-space>
              </td>
            </tr>
          </tbody>
        </n-table>
      </SectionCard>
    </n-tab-pane>

    <!-- 用户偏好 -->
    <n-tab-pane name="pref" tab="偏好设置">
      <SectionCard :hoverable="false">
        <n-form v-if="pref" label-placement="left" label-width="120" style="max-width: 480px">
          <n-form-item label="风险等级">
            <n-select v-model:value="pref.risk_level" :options="riskOptions" />
          </n-form-item>
          <n-form-item label="默认市场">
            <n-select v-model:value="pref.default_market" :options="marketOptions" />
          </n-form-item>
          <n-form-item label="默认周期">
            <n-select v-model:value="pref.horizon_pref" :options="horizonOptions" />
          </n-form-item>
          <n-form-item label="默认推荐数量">
            <n-input-number v-model:value="pref.default_rec_count" :min="3" :max="5" />
          </n-form-item>
          <n-form-item label="开启提醒">
            <div class="notify-switch">
              <n-switch v-model:value="pref.enable_notify" />
              <span class="notify-hint">推送总闸：关闭后提醒命中仅在站内展示，不再外推到 Server酱/Webhook</span>
            </div>
          </n-form-item>
          <n-form-item label="成交额门槛">
            <div class="notify-switch">
              <n-input-number v-model:value="minAmountYi" :min="0" :max="10000" :precision="2" :step="0.5" style="width: 140px">
                <template #suffix>亿元</template>
              </n-input-number>
              <span class="notify-hint">推荐候选池剔除日成交额低于该值的标的；0 = 不过滤</span>
            </div>
          </n-form-item>
          <n-form-item label="候选池黑名单">
            <div class="blacklist">
              <div v-for="(b, i) in blacklist" :key="b.market + ':' + b.symbol" class="black-row">
                <span class="qv-mono black-symbol">{{ b.symbol }}</span>
                <span class="black-reason">{{ b.reason || '—' }}</span>
                <n-button size="tiny" quaternary type="error" @click="removeBlack(i)">移除</n-button>
              </div>
              <div class="black-add">
                <n-input v-model:value="newBlack.symbol" placeholder="代码，如 600000" size="small" style="width: 140px" />
                <n-input v-model:value="newBlack.reason" placeholder="回避原因（可选）" size="small" style="flex: 1" @keyup.enter="addBlack" />
                <n-button size="small" @click="addBlack">加入</n-button>
              </div>
              <span class="notify-hint">生成推荐时黑名单标的将从候选池剔除（随「保存偏好」生效）</span>
            </div>
          </n-form-item>
          <n-button type="primary" :loading="savingPref" @click="savePref">保存偏好</n-button>
        </n-form>
      </SectionCard>
      <SectionCard v-if="quota" title="AI 用量" :hoverable="false" style="margin-top: 16px">
        <div class="quota">
          <span>累计消耗 token：<b class="qv-tnum">{{ quota.token_used.toLocaleString() }}</b></span>
          <span>调用次数：<b class="qv-tnum">{{ quota.request_count }}</b></span>
          <span v-if="quota.token_limit > 0"
            >额度：<b class="qv-tnum">{{ quota.token_limit.toLocaleString() }}</b>（用尽后 AI 功能将被熔断）</span
          >
          <span v-else>额度：不限</span>
        </div>
      </SectionCard>
    </n-tab-pane>

    <!-- 账号安全 -->
    <n-tab-pane name="account" tab="账号安全">
      <SectionCard title="修改密码" :hoverable="false">
        <n-form label-placement="left" label-width="120" style="max-width: 480px">
          <n-form-item label="原密码">
            <n-input v-model:value="pw.old" type="password" show-password-on="click" placeholder="纯 GitHub 账号首次设密码可留空" />
          </n-form-item>
          <n-form-item label="新密码">
            <n-input v-model:value="pw.next" type="password" show-password-on="click" placeholder="至少 8 个字符" />
          </n-form-item>
          <n-form-item label="确认新密码">
            <n-input v-model:value="pw.confirm" type="password" show-password-on="click" @keyup.enter="submitChangePassword" />
          </n-form-item>
          <n-button type="primary" :loading="savingPw" @click="submitChangePassword">修改密码</n-button>
        </n-form>
      </SectionCard>
      <SectionCard title="数据导出" :hoverable="false" style="margin-top: 16px">
        <div class="export-row">
          <n-button
            v-for="opt in exportOptions"
            :key="opt.kind"
            ghost
            :loading="exporting === opt.kind"
            @click="doExport(opt.kind)"
            >{{ opt.label }}</n-button
          >
        </div>
        <div class="export-hint">导出为 CSV（带 BOM，Excel 双击可读中文），仅含当前账号数据。</div>
      </SectionCard>
    </n-tab-pane>
    </n-tabs>
  </PageContainer>

  <!-- 新增/编辑配置弹窗 -->
  <n-modal v-model:show="showModal" preset="card" :title="editingId ? '编辑 LLM 配置' : '新增 LLM 配置'" style="max-width: 520px">
    <n-form label-placement="left" label-width="96">
      <n-form-item label="名称">
        <n-input v-model:value="form.name" placeholder="如 我的 DeepSeek" />
      </n-form-item>
      <n-form-item label="类型">
        <n-select v-model:value="form.provider" :options="providerOptions" />
      </n-form-item>
      <n-form-item label="Base URL">
        <n-input v-model:value="form.base_url" placeholder="如 https://api.deepseek.com/v1" />
      </n-form-item>
      <n-form-item label="API Key">
        <n-input
          v-model:value="form.api_key"
          type="password"
          show-password-on="click"
          :placeholder="editingId ? '留空表示保留原密钥' : 'sk-...'"
        />
      </n-form-item>
      <n-form-item label="模型">
        <n-input v-model:value="form.model" placeholder="如 deepseek-chat" />
      </n-form-item>
      <n-form-item label="Temperature">
        <n-input-number v-model:value="form.temperature" :min="0" :max="2" :step="0.1" />
      </n-form-item>
      <n-form-item label="Max Tokens">
        <n-input-number v-model:value="form.max_tokens" :min="1" :max="200000" />
      </n-form-item>
      <n-form-item label="流式输出">
        <n-switch v-model:value="form.stream" />
      </n-form-item>
      <n-form-item label="设为默认">
        <n-switch v-model:value="form.is_default" />
      </n-form-item>
    </n-form>
    <template #footer>
      <n-space justify="space-between">
        <n-button :loading="testing" @click="testDraft">测试连接</n-button>
        <n-space>
          <n-button @click="showModal = false">取消</n-button>
          <n-button type="primary" :loading="saving" @click="save">保存</n-button>
        </n-space>
      </n-space>
    </template>
  </n-modal>
</template>

<style scoped>
.export-row {
  display: flex;
  gap: 10px;
  flex-wrap: wrap;
}
.export-hint {
  font-size: 12px;
  opacity: 0.55;
  margin-top: 10px;
}
.card-toolbar {
  display: flex;
  align-items: center;
  justify-content: space-between;
  margin-bottom: 14px;
}
.ct-title {
  font-size: 14px;
  font-weight: 600;
}
.notify-switch {
  display: flex;
  align-items: center;
  gap: 10px;
}
.notify-hint {
  font-size: 12px;
  opacity: 0.65;
}
.blacklist {
  display: flex;
  flex-direction: column;
  gap: 8px;
  width: 100%;
}
.black-row {
  display: flex;
  align-items: center;
  gap: 12px;
  font-size: 13px;
}
.black-symbol {
  min-width: 70px;
}
.black-reason {
  flex: 1;
  opacity: 0.7;
}
.black-add {
  display: flex;
  align-items: center;
  gap: 8px;
}
.quota {
  display: flex;
  flex-wrap: wrap;
  gap: 18px;
  font-size: 13px;
}
</style>
