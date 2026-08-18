<script setup lang="ts">
import { computed, onMounted, reactive, ref } from 'vue'
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
  getBrowserNotificationConfig,
  updateBrowserNotificationSettings,
  upsertBrowserSubscription,
  removeBrowserDevice,
  testBrowserNotification,
  type NotifyChannel,
  type NotifyKind,
  type BrowserNotificationConfig,
  type BrowserNotificationDevice,
} from '@/api/notify'
import SectionCard from '@/components/SectionCard.vue'
import { useUi } from '@/composables/useUi'
import { useAuthStore } from '@/stores/auth'
import {
  browserDeviceKey,
  browserNotificationSupported,
  browserPermission,
  currentBrowserDeviceID,
  ensureNotificationServiceWorker,
  pushSubscriptionInput,
  rememberBrowserDeviceID,
  urlBase64ToUint8Array,
  webPushSupported,
} from '@/composables/useBrowserNotifications'

const message = useMessage()
const { vars } = useUi()
const auth = useAuthStore()

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

// ---------- 浏览器通知 ----------
const browserConfig = ref<BrowserNotificationConfig | null>(null)
const browserLoading = ref(false)
const browserSaving = ref(false)
const browserError = ref('')
const permission = ref(browserPermission())
const browserSupported = browserNotificationSupported()
const pushSupported = webPushSupported()
const secureContext = typeof window !== 'undefined' && window.isSecureContext
const currentDeviceID = ref<number | null>(currentBrowserDeviceID(auth.user?.id || 0))
const currentDevice = computed(() => browserConfig.value?.devices.find((item) => item.id === currentDeviceID.value) || null)
const deviceActive = computed(() => permission.value === 'granted' && !!currentDevice.value)
const permissionLabel = computed(() => {
  if (permission.value === 'unsupported') return '不支持'
  if (permission.value === 'granted') return '已允许'
  if (permission.value === 'denied') return '已拒绝'
  return '未询问'
})

async function loadBrowserConfig() {
  browserLoading.value = true
  browserError.value = ''
  permission.value = browserPermission()
  try {
    browserConfig.value = await getBrowserNotificationConfig()
    if (currentDeviceID.value && !browserConfig.value.devices.some((item) => item.id === currentDeviceID.value)) {
      currentDeviceID.value = null
      rememberBrowserDeviceID(auth.user?.id || 0, null)
    }
  } catch {
    browserError.value = browserConfig.value ? '浏览器通知状态刷新失败，继续保留上次数据。' : '浏览器通知状态加载失败，请重试。'
  } finally {
    browserLoading.value = false
  }
}

function deviceName() {
  const platform = (navigator as Navigator & { userAgentData?: { platform?: string } }).userAgentData?.platform || navigator.platform || '浏览器'
  return `${platform} · ${new Date().toLocaleDateString('zh-CN')}`
}

async function enableBrowserNotification(force = false) {
  if (browserSaving.value) return
  if (!browserSupported) {
    browserError.value = '当前浏览器不支持 Notification API。'
    return
  }
  if (!secureContext) {
    browserError.value = '浏览器通知只能在 HTTPS 或 localhost 环境使用。'
    return
  }
  browserSaving.value = true
  browserError.value = ''
  try {
    // 权限申请严格位于用户点击处理函数中，页面加载时绝不调用。
    const result = Notification.permission === 'granted' ? 'granted' : await Notification.requestPermission()
    permission.value = result
    if (result !== 'granted') {
      browserError.value = result === 'denied'
        ? '通知权限已被浏览器拒绝。请在地址栏站点设置中改为“允许”后重新订阅。'
        : '未获得通知权限，浏览器通知没有开启。'
      return
    }

    const registration = await ensureNotificationServiceWorker()
    let pushInput: { endpoint?: string; p256dh?: string; auth?: string } = {}
    let pushFallback = false
    if (browserConfig.value?.vapid_configured && pushSupported) {
      try {
        let subscription = await registration.pushManager.getSubscription()
        if (force && subscription) {
          await subscription.unsubscribe()
          subscription = null
        }
        if (!subscription) {
          subscription = await registration.pushManager.subscribe({
            userVisibleOnly: true,
            applicationServerKey: urlBase64ToUint8Array(browserConfig.value.vapid_public_key),
          })
        }
        pushInput = pushSubscriptionInput(subscription)
      } catch {
        pushFallback = true
      }
    }

    const saved = await upsertBrowserSubscription({
      device_key: browserDeviceKey(auth.user?.id || 0),
      name: deviceName(),
      ...pushInput,
    })
    currentDeviceID.value = saved.id
    rememberBrowserDeviceID(auth.user?.id || 0, saved.id)
    await Promise.all([loadBrowserConfig(), loadPreference()])
    if (pushFallback) message.warning('Web Push 订阅失败，已保留网站打开期间的浏览器通知。')
    else message.success(saved.has_web_push ? '浏览器通知和 Web Push 已开启' : '网站打开期间的浏览器通知已开启')
  } catch (error) {
    browserError.value = (error as Error).message || '浏览器通知开启失败，请重试。'
  } finally {
    browserSaving.value = false
  }
}

async function removeDevice(device: BrowserNotificationDevice) {
  try {
    await removeBrowserDevice(device.id)
    if (device.id === currentDeviceID.value) {
      if ('serviceWorker' in navigator) {
        const registration = await navigator.serviceWorker.getRegistration('/')
        const subscription = await registration?.pushManager.getSubscription()
        await subscription?.unsubscribe().catch(() => false)
      }
      currentDeviceID.value = null
      rememberBrowserDeviceID(auth.user?.id || 0, null)
    }
    await loadBrowserConfig()
    message.success('浏览器通知设备已移除')
  } catch {
    browserError.value = '设备移除失败，原订阅仍保留。'
  }
}

async function saveBrowserSettings() {
  if (!browserConfig.value) return
  browserSaving.value = true
  try {
    browserConfig.value.settings = await updateBrowserNotificationSettings(browserConfig.value.settings)
    message.success('浏览器通知分类已保存')
  } catch {
    browserError.value = '浏览器通知分类保存失败，请重试。'
  } finally {
    browserSaving.value = false
  }
}

async function sendBrowserTest() {
  if (!currentDevice.value) return
  try {
    await testBrowserNotification(browserDeviceKey(auth.user?.id || 0))
    message.success('测试通知已发送')
  } catch {
    browserError.value = '测试通知失败，请重新订阅后重试。'
  }
}

onMounted(() => {
  void loadPreference()
  void loadChannels()
  void loadBrowserConfig()
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
              <span>关闭后提醒仍在站内保留，但不会发往浏览器、Server酱、Webhook 或 ntfy。</span>
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

    <SectionCard title="浏览器通知" :hoverable="false">
      <n-spin :show="browserLoading && !browserConfig">
        <n-alert v-if="browserError" type="warning" title="浏览器通知需要处理" :bordered="false">
          {{ browserError }}
          <div class="recover-row"><n-button size="small" @click="loadBrowserConfig">重新加载</n-button></div>
        </n-alert>

        <div class="browser-status-grid">
          <div><span>浏览器支持</span><n-tag size="small" :type="browserSupported ? 'success' : 'default'">{{ browserSupported ? '支持' : '不支持' }}</n-tag></div>
          <div><span>通知权限</span><n-tag size="small" :type="permission === 'granted' ? 'success' : permission === 'denied' ? 'error' : 'warning'">{{ permissionLabel }}</n-tag></div>
          <div><span>Web Push 服务</span><n-tag size="small" :type="browserConfig?.vapid_configured ? 'success' : 'default'">{{ browserConfig?.vapid_configured ? '已配置' : '未配置' }}</n-tag></div>
          <div><span>当前设备</span><n-tag size="small" :type="deviceActive ? 'success' : 'default'">{{ deviceActive ? (currentDevice?.has_web_push ? '已订阅 Web Push' : '仅前台通知') : '未订阅' }}</n-tag></div>
        </div>

        <n-alert type="info" :bordered="false" class="browser-limit">
          需要 HTTPS 或 localhost。网站打开时可通过 Notification API 和事件轮询提醒；网站关闭后还需要浏览器支持 Web Push 且服务端配置 VAPID。iPhone/iPad 通常需先将网站添加到主屏幕，再从主屏幕打开后授权。
        </n-alert>
        <n-alert v-if="permission === 'denied'" type="warning" :bordered="false" class="browser-limit">
          当前权限已拒绝，页面无法再次弹出权限框。请打开地址栏旁的站点设置，将“通知”改为允许后再点重新订阅。
        </n-alert>

        <div class="browser-actions">
          <n-button v-if="!deviceActive" type="primary" :loading="browserSaving" @click="enableBrowserNotification(false)">开启浏览器通知</n-button>
          <template v-else>
            <n-button type="primary" :loading="browserSaving" @click="enableBrowserNotification(true)">重新订阅</n-button>
            <n-button :loading="browserSaving" @click="sendBrowserTest">发送测试通知</n-button>
            <n-button @click="currentDevice && removeDevice(currentDevice)">关闭当前设备</n-button>
          </template>
        </div>

        <n-form v-if="browserConfig" label-placement="top" :show-feedback="false" class="browser-category-form">
          <n-form-item label="通知分类">
            <div class="switch-grid">
              <label><n-switch v-model:value="browserConfig.settings.exit_risk" size="small" /> 持仓卖出风险</label>
              <label><n-switch v-model:value="browserConfig.settings.manual_alert" size="small" /> 手工提醒规则</label>
              <label><n-switch v-model:value="browserConfig.settings.guard" size="small" /> 智能守护事件</label>
            </div>
          </n-form-item>
          <n-button :loading="browserSaving" @click="saveBrowserSettings">保存通知分类</n-button>
        </n-form>

        <div v-if="browserConfig?.devices.length" class="browser-devices">
          <div class="devices-heading">已订阅设备</div>
          <div v-for="device in browserConfig.devices" :key="device.id" class="channel-row">
            <div class="channel-main">
              <strong>{{ device.name }}</strong>
              <span class="channel-meta">
                {{ device.has_web_push ? 'Web Push' : '仅网站打开期间' }}
                <template v-if="device.last_seen_at"> · 最近活动 {{ new Date(device.last_seen_at).toLocaleString('zh-CN', { hour12: false }) }}</template>
              </span>
              <n-alert v-if="device.last_error_code" type="warning" :bordered="false">上次 Web Push 失败，建议重新订阅。</n-alert>
            </div>
            <n-popconfirm @positive-click="removeDevice(device)">
              <template #trigger><n-button size="small" type="error" quaternary>移除设备</n-button></template>
              移除“{{ device.name }}”的浏览器通知？
            </n-popconfirm>
          </div>
        </div>
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
              <n-input v-model:value="ntfyForm.url" :placeholder="editingID ? '留空保留原服务地址' : 'https://ntfy.example.com'" />
            </n-form-item>
            <n-form-item label="Topic">
              <n-input v-model:value="ntfyForm.topic" :placeholder="editingID ? '留空保留原 Topic' : '如 qv-u1'" />
            </n-form-item>
            <n-form-item label="访问令牌">
              <n-input v-model:value="ntfyForm.token" type="password" show-password-on="click" :placeholder="editingID ? '留空保留原令牌' : '可选，不会回显已保存密钥'" />
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
        <p class="channel-hint">密钥和敏感地址只在保存时提交，服务端加密存储且不会回显。编辑 ntfy 时三项全部留空会保留原配置；如需更换，请重新填写完整的服务地址和 Topic。</p>
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
.browser-status-grid {
  display: grid;
  grid-template-columns: repeat(4, minmax(0, 1fr));
  gap: 10px;
  margin-bottom: 12px;
}
.browser-status-grid > div {
  display: flex;
  justify-content: space-between;
  align-items: center;
  gap: 8px;
  min-width: 0;
  padding: 10px 0;
  border-bottom: 1px solid v-bind('vars.dividerColor');
}
.browser-limit {
  margin: 10px 0;
}
.browser-actions {
  display: flex;
  flex-wrap: wrap;
  gap: 8px;
  margin: 12px 0;
}
.browser-category-form {
  max-width: 720px;
  margin-top: 16px;
}
.browser-devices {
  display: grid;
  margin-top: 18px;
}
.devices-heading {
  font-weight: 600;
  margin-bottom: 4px;
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
  .browser-status-grid {
    grid-template-columns: 1fr 1fr;
  }
}
@media (max-width: 420px) {
  .browser-status-grid {
    grid-template-columns: 1fr;
  }
}
</style>
