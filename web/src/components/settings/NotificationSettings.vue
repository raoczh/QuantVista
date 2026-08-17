<script setup lang="ts">
import { onMounted, reactive, ref } from 'vue'
import {
  NAlert,
  NButton,
  NEmpty,
  NForm,
  NFormItem,
  NInput,
  NInputNumber,
  NPopconfirm,
  NSelect,
  NSpin,
  NSwitch,
  NTag,
  useMessage,
} from 'naive-ui'
import { getPreference, updatePreference, type UserPreference } from '@/api/user'
import {
  createChannel,
  deleteChannel,
  listChannels,
  testChannel,
  updateChannel,
  type NotifyChannel,
  type NotifyKind,
} from '@/api/notify'
import SectionCard from '@/components/SectionCard.vue'
import { useUi } from '@/composables/useUi'

const message = useMessage()
const { vars } = useUi()

const preference = ref<UserPreference | null>(null)
const preferenceLoading = ref(false)
const preferenceSaving = ref(false)
const preferenceError = ref('')

const guard = reactive({
  enabled: true,
  pos_pct: 5,
  watch_pct: 7,
  stop_loss: true,
  take_profit: true,
  evening: true,
})

function parseGuard(raw: string) {
  if (!raw) return
  try {
    const value = JSON.parse(raw)
    if (value && typeof value === 'object') Object.assign(guard, value)
  } catch {
    preferenceError.value = '智能守护设置格式异常，当前展示默认值；保存前请核对。'
  }
}

async function loadPreference() {
  preferenceLoading.value = true
  preferenceError.value = ''
  try {
    const value = await getPreference()
    preference.value = value
    parseGuard(value.guard_config_json)
  } catch {
    preferenceError.value = preference.value
      ? '通知设置刷新失败，继续保留上次加载的数据。'
      : '通知设置加载失败，请重试。'
  } finally {
    preferenceLoading.value = false
  }
}

async function savePreference() {
  if (!preference.value) return
  preferenceSaving.value = true
  preferenceError.value = ''
  try {
    const latest = await getPreference()
    const payload = {
      ...latest,
      enable_notify: preference.value.enable_notify,
      guard_config_json: JSON.stringify(guard),
    }
    const saved = await updatePreference(payload)
    preference.value = saved
    parseGuard(saved.guard_config_json)
    message.success('通知设置已保存')
  } catch {
    preferenceError.value = '通知设置保存失败，已填写内容仍保留，可直接重试。'
  } finally {
    preferenceSaving.value = false
  }
}

const channels = ref<NotifyChannel[]>([])
const channelsLoading = ref(false)
const channelsError = ref('')
const channelSaving = ref(false)
const editingID = ref<number | null>(null)
const channelForm = reactive<{ kind: NotifyKind; name: string; target: string; enabled: boolean }>({
  kind: 'serverchan',
  name: '',
  target: '',
  enabled: true,
})
const ntfyForm = reactive({ url: '', topic: '', token: '' })

const channelTypeOptions = [
  { label: 'Server酱', value: 'serverchan' },
  { label: 'Webhook', value: 'webhook' },
  { label: 'ntfy', value: 'ntfy' },
]

function channelTypeLabel(kind: NotifyKind) {
  if (kind === 'serverchan') return 'Server酱'
  if (kind === 'ntfy') return 'ntfy'
  return 'Webhook'
}

function resetChannelForm() {
  editingID.value = null
  Object.assign(channelForm, { kind: 'serverchan', name: '', target: '', enabled: true })
  Object.assign(ntfyForm, { url: '', topic: '', token: '' })
}

function editChannel(channel: NotifyChannel) {
  editingID.value = channel.id
  Object.assign(channelForm, {
    kind: channel.kind,
    name: channel.name,
    target: '',
    enabled: channel.enabled,
  })
  Object.assign(ntfyForm, { url: '', topic: '', token: '' })
}

async function loadChannels() {
  channelsLoading.value = true
  channelsError.value = ''
  try {
    channels.value = await listChannels()
  } catch {
    channelsError.value = channels.value.length
      ? '通道刷新失败，继续保留上次加载的数据。'
      : '推送通道加载失败，请重试。'
  } finally {
    channelsLoading.value = false
  }
}

function buildTarget(): string | null {
  if (channelForm.kind === 'ntfy') {
    const anyNtfyInput = ntfyForm.url.trim() || ntfyForm.topic.trim() || ntfyForm.token.trim()
    if (editingID.value && !anyNtfyInput) return ''
    if (!ntfyForm.url.trim() || !ntfyForm.topic.trim()) {
      message.warning('请填写 ntfy 服务地址与 Topic')
      return null
    }
    return JSON.stringify({
      url: ntfyForm.url.trim(),
      topic: ntfyForm.topic.trim(),
      token: ntfyForm.token.trim(),
    })
  }
  const target = channelForm.target.trim()
  if (editingID.value && !target) return ''
  if (!target) {
    message.warning(channelForm.kind === 'serverchan' ? '请填写 Server酱 SendKey' : '请填写 Webhook 地址')
    return null
  }
  return target
}

async function saveChannel() {
  if (channelSaving.value) return
  const target = buildTarget()
  if (target === null) return
  channelSaving.value = true
  channelsError.value = ''
  try {
    const payload = {
      kind: channelForm.kind,
      name: channelForm.name.trim(),
      target,
      enabled: channelForm.enabled,
    }
    if (editingID.value) await updateChannel(editingID.value, payload)
    else await createChannel(payload)
    message.success(editingID.value ? '推送通道已更新' : '推送通道已添加')
    resetChannelForm()
    await Promise.all([loadChannels(), loadPreference()])
  } catch {
    channelsError.value = '推送通道保存失败，已填写内容和现有通道仍保留。'
  } finally {
    channelSaving.value = false
  }
}

async function toggleChannel(channel: NotifyChannel) {
  try {
    await updateChannel(channel.id, {
      kind: channel.kind,
      name: channel.name,
      enabled: !channel.enabled,
    })
    await loadChannels()
  } catch {
    channelsError.value = '通道状态更新失败，现有状态未改动。'
  }
}

async function testSavedChannel(channel: NotifyChannel) {
  try {
    await testChannel(channel.id)
    message.success('测试推送已发送，请在对应客户端查收')
    await loadChannels()
  } catch {
    channelsError.value = '测试推送失败，请检查通道配置后重试。'
  }
}

async function removeChannel(channel: NotifyChannel) {
  try {
    await deleteChannel(channel.id)
    if (editingID.value === channel.id) resetChannelForm()
    await loadChannels()
    message.success('推送通道已删除')
  } catch {
    channelsError.value = '推送通道删除失败，原通道仍保留。'
  }
}

onMounted(() => {
  void loadPreference()
  void loadChannels()
})
</script>

<template>
  <div class="notification-settings">
    <SectionCard title="通知总览" :hoverable="false">
      <n-spin :show="preferenceLoading && !preference">
        <n-alert v-if="preferenceError" type="warning" title="通知设置需要处理" :bordered="false">
          {{ preferenceError }}
          <div class="recover-row">
            <n-button size="small" :loading="preferenceLoading" @click="loadPreference">重新加载</n-button>
            <n-button v-if="preference" size="small" type="primary" :loading="preferenceSaving" @click="savePreference">重试保存</n-button>
          </div>
        </n-alert>
        <n-form v-if="preference" label-placement="top" :show-feedback="false" class="notify-form">
          <n-form-item label="推送总闸">
            <div class="switch-row">
              <n-switch v-model:value="preference.enable_notify" />
              <span>关闭后提醒仍在站内保留，但不会发往 Server酱、Webhook 或 ntfy。</span>
            </div>
          </n-form-item>
          <n-form-item label="智能守护">
            <div class="guard-settings">
              <div class="switch-row">
                <n-switch v-model:value="guard.enabled" />
                <span>交易时段检查持仓和重点自选，盘后检查持仓事件；通知仍受推送总闸控制。</span>
              </div>
              <template v-if="guard.enabled">
                <div class="guard-row">
                  <span>持仓异动阈值</span>
                  <n-input-number v-model:value="guard.pos_pct" :min="1" :max="30" :precision="1" :step="0.5" size="small">
                    <template #suffix>%</template>
                  </n-input-number>
                </div>
                <div class="guard-row">
                  <span>重点自选异动阈值</span>
                  <n-input-number v-model:value="guard.watch_pct" :min="1" :max="30" :precision="1" :step="0.5" size="small">
                    <template #suffix>%</template>
                  </n-input-number>
                </div>
                <div class="switch-grid">
                  <label><n-switch v-model:value="guard.stop_loss" size="small" /> 止损触达</label>
                  <label><n-switch v-model:value="guard.take_profit" size="small" /> 止盈触达</label>
                  <label><n-switch v-model:value="guard.evening" size="small" /> 持仓盘后事件</label>
                </div>
              </template>
            </div>
          </n-form-item>
          <n-button type="primary" :loading="preferenceSaving" @click="savePreference">保存通知设置</n-button>
        </n-form>
      </n-spin>
    </SectionCard>

    <SectionCard title="推送通道" :hoverable="false">
      <n-alert v-if="channelsError" type="warning" title="推送通道需要处理" :bordered="false">
        {{ channelsError }}
        <div class="recover-row">
          <n-button size="small" :loading="channelsLoading" @click="loadChannels">重新加载</n-button>
        </div>
      </n-alert>

      <n-form label-placement="top" :show-feedback="false" class="channel-form">
        <div class="channel-fields">
          <n-form-item label="通道类型">
            <n-select v-model:value="channelForm.kind" :options="channelTypeOptions" :disabled="editingID !== null" />
          </n-form-item>
          <n-form-item label="显示名称">
            <n-input v-model:value="channelForm.name" placeholder="留空使用默认名称" />
          </n-form-item>
        </div>
        <template v-if="channelForm.kind === 'ntfy'">
          <div class="channel-fields ntfy-fields">
            <n-form-item label="ntfy 服务地址">
              <n-input v-model:value="ntfyForm.url" placeholder="https://ntfy.example.com" />
            </n-form-item>
            <n-form-item label="Topic">
              <n-input v-model:value="ntfyForm.topic" placeholder="如 qv-u1" />
            </n-form-item>
            <n-form-item label="访问令牌">
              <n-input v-model:value="ntfyForm.token" type="password" show-password-on="click" placeholder="可选，不会回显已保存密钥" />
            </n-form-item>
          </div>
        </template>
        <n-form-item v-else :label="channelForm.kind === 'serverchan' ? 'SendKey' : 'Webhook 地址'">
          <n-input
            v-model:value="channelForm.target"
            type="password"
            show-password-on="click"
            :placeholder="editingID ? '留空表示保留原密钥或地址' : channelForm.kind === 'serverchan' ? 'Server酱 SendKey' : 'https://...'"
          />
        </n-form-item>
        <div class="form-actions">
          <n-button type="primary" :loading="channelSaving" @click="saveChannel">{{ editingID ? '保存修改' : '添加通道' }}</n-button>
          <n-button v-if="editingID" @click="resetChannelForm">取消编辑</n-button>
        </div>
        <p class="channel-hint">密钥只在保存时提交，服务端加密存储且不会回显。编辑时留空会保留原值。</p>
      </n-form>

      <n-spin :show="channelsLoading && !channels.length">
        <n-empty v-if="!channels.length && !channelsLoading" description="还没有推送通道" />
        <div v-else class="channels">
          <div v-for="channel in channels" :key="channel.id" class="channel-row">
            <div class="channel-main">
              <div class="channel-title">
                <n-tag size="small" :bordered="false">{{ channelTypeLabel(channel.kind) }}</n-tag>
                <strong>{{ channel.name }}</strong>
                <n-tag size="small" :type="channel.enabled ? 'success' : 'default'" :bordered="false">{{ channel.enabled ? '已启用' : '已停用' }}</n-tag>
              </div>
              <span v-if="channel.last_sent_at" class="channel-meta">最近发送：{{ new Date(channel.last_sent_at).toLocaleString('zh-CN', { hour12: false }) }}</span>
              <n-alert v-if="channel.last_error" type="error" :bordered="false" class="channel-error">
                上次推送失败。请测试通道；如仍失败，编辑并更新地址或密钥。
              </n-alert>
            </div>
            <div class="channel-actions">
              <n-button size="small" @click="testSavedChannel(channel)">测试</n-button>
              <n-button size="small" @click="editChannel(channel)">编辑</n-button>
              <n-button size="small" @click="toggleChannel(channel)">{{ channel.enabled ? '停用' : '启用' }}</n-button>
              <n-popconfirm @positive-click="removeChannel(channel)">
                <template #trigger><n-button size="small" type="error" quaternary>删除</n-button></template>
                删除推送通道“{{ channel.name }}”？
              </n-popconfirm>
            </div>
          </div>
        </div>
      </n-spin>
    </SectionCard>
  </div>
</template>

<style scoped>
.notification-settings {
  display: grid;
  gap: 16px;
}
.notify-form {
  max-width: 720px;
}
.switch-row {
  display: flex;
  align-items: center;
  gap: 10px;
  line-height: 1.55;
}
.guard-settings {
  display: grid;
  gap: 12px;
  width: 100%;
}
.guard-row {
  display: grid;
  grid-template-columns: minmax(140px, 1fr) 140px;
  align-items: center;
  gap: 10px;
  max-width: 380px;
}
.switch-grid {
  display: flex;
  flex-wrap: wrap;
  gap: 12px 20px;
}
.switch-grid label {
  display: inline-flex;
  align-items: center;
  gap: 7px;
}
.channel-form {
  max-width: 820px;
  padding-bottom: 18px;
  border-bottom: 1px solid v-bind('vars.dividerColor');
}
.channel-fields {
  display: grid;
  grid-template-columns: repeat(2, minmax(0, 1fr));
  gap: 12px;
}
.ntfy-fields {
  grid-template-columns: repeat(3, minmax(0, 1fr));
}
.form-actions,
.recover-row,
.channel-actions,
.channel-title {
  display: flex;
  align-items: center;
  flex-wrap: wrap;
  gap: 8px;
}
.recover-row {
  margin-top: 9px;
}
.channel-hint,
.channel-meta {
  font-size: 12px;
  opacity: 0.62;
}
.channels {
  display: grid;
}
.channel-row {
  display: flex;
  align-items: flex-start;
  gap: 16px;
  padding: 14px 0;
  border-bottom: 1px solid v-bind('vars.dividerColor');
}
.channel-row:last-child {
  border-bottom: 0;
}
.channel-main {
  display: grid;
  flex: 1;
  min-width: 0;
  gap: 5px;
}
.channel-actions {
  justify-content: flex-end;
}
.channel-error {
  margin-top: 3px;
}
@media (max-width: 680px) {
  .channel-fields,
  .ntfy-fields {
    grid-template-columns: 1fr;
    gap: 0;
  }
  .channel-row {
    flex-direction: column;
  }
  .channel-actions {
    justify-content: flex-start;
    width: 100%;
  }
  .switch-row {
    align-items: flex-start;
  }
  .guard-row {
    grid-template-columns: 1fr;
    gap: 5px;
  }
}
</style>
