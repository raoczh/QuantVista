<script setup lang="ts">
import { computed, h } from 'vue'
import { NCard, NDropdown, NButton, NIcon } from 'naive-ui'
import { storeToRefs } from 'pinia'
import { useThemeStore } from '@/stores/theme'
import { useUi } from '@/composables/useUi'
import BrandLogo from '@/components/BrandLogo.vue'
import WorkspaceIcon from '@/components/WorkspaceIcon.vue'

withDefaults(defineProps<{ subtitle?: string; description?: string }>(), {
  subtitle: 'AI 股票研究平台', description: '连接你的研究、关注与持仓。',
})

const themeStore = useThemeStore()
const { currentKey, preset } = storeToRefs(themeStore)
const { vars, primaryAlpha } = useUi()

const bgStyle = computed(() => ({
  '--auth-accent': primaryAlpha(.07),
  '--auth-border': vars.value.dividerColor,
  background: vars.value.bodyColor,
}))

const themeOptions = computed(() =>
  themeStore.presets.map((p) => ({
    key: p.key,
    label: p.label,
    icon: () =>
      h('span', {
        style: `display:inline-block;width:14px;height:14px;border-radius:4px;background:${p.primary};border:1px solid rgba(128,128,128,.4)`,
      }),
  })),
)
function onSelectTheme(key: string) {
  themeStore.setTheme(key)
}
</script>

<template>
  <div class="auth-shell" :style="bgStyle">
    <div class="auth-topbar">
      <n-dropdown trigger="click" :options="themeOptions" :value="currentKey" @select="onSelectTheme">
        <n-button quaternary size="small">
          <template #icon>
            <n-icon>
              <span :style="`display:inline-block;width:14px;height:14px;border-radius:4px;background:${preset.primary}`" />
            </n-icon>
          </template>
          {{ preset.label }}
        </n-button>
      </n-dropdown>
    </div>

    <main class="auth-body">
      <section class="auth-intro" aria-label="关于 QuantVista">
        <BrandLogo :size="36" />
        <div class="auth-story">
          <p class="auth-eyebrow">你的个人投研工作台</p>
          <h2>让每一次判断，<br />都有依据。</h2>
          <p class="auth-description">从发现机会到持仓复盘，把数据、研究和行动放在一起。</p>
          <ol class="auth-steps">
            <li><WorkspaceIcon name="compass" /><div><strong>发现候选</strong><span>从市场和策略中整理关注名单</span></div></li>
            <li><WorkspaceIcon name="layers" /><div><strong>核对证据</strong><span>结合 AI 分析，分清事实与待验证的判断</span></div></li>
            <li><WorkspaceIcon name="history" /><div><strong>持续复盘</strong><span>跟踪持仓风险，记录每次决策的后续表现</span></div></li>
          </ol>
        </div>
        <p class="auth-footnote">研究参考 · 独立判断 · 风险自担</p>
      </section>
      <section class="auth-form-area">
        <div class="auth-brand"><BrandLogo :size="34" /></div>
        <n-card class="auth-card">
          <div class="auth-form-heading"><h1>{{ subtitle }}</h1><p v-if="description">{{ description }}</p></div>
          <slot />
        </n-card>
      </section>
    </main>
  </div>
</template>

<style scoped>
.auth-shell {
  box-sizing: border-box;
  position: relative;
  min-height: 100vh;
  display: flex;
  align-items: center;
  justify-content: center;
  padding: 72px 32px 48px;
}
.auth-topbar {
  position: absolute;
  top: 16px;
  right: 20px;
}
.auth-body {
  width: 100%;
  max-width: 1060px;
  display: grid;
  grid-template-columns: minmax(0, 1.1fr) minmax(340px, .9fr);
  align-items: center;
  gap: 80px;
}
.auth-brand {
  display: none;
}
.auth-intro { min-width: 0; }
.auth-story { margin-top: 62px; }
.auth-eyebrow { margin: 0 0 14px; font-size: 12px; color: var(--qv-primary); font-weight: 600; letter-spacing: .1em; }
.auth-story h2 { margin: 0; font-size: clamp(32px, 3.1vw, 43px); line-height: 1.4; font-weight: 650; letter-spacing: -.04em; }
.auth-description { margin: 20px 0 28px; max-width: 32em; color: var(--qv-text-secondary); font-size: 14px; line-height: 1.9; }
.auth-steps { display: grid; gap: 21px; list-style: none; padding: 0; margin: 30px 0; }
.auth-steps li { display: flex; gap: 14px; align-items: center; }
.auth-steps svg { width: 38px; height: 38px; flex: 0 0 38px; padding: 10px; border-radius: 10px; background: var(--auth-accent); color: var(--qv-primary); }
.auth-steps strong { display: block; font-size: 13px; font-weight: 600; }
.auth-steps span, .auth-footnote { font-size: 12px; color: var(--qv-text-muted); }
.auth-footnote { margin: 46px 0 0; }
.auth-form-area { min-width: 0; }
.auth-card {
  border-radius: 16px;
  border-color: var(--auth-border);
  padding: 12px;
  box-shadow: 0 12px 42px rgba(0, 0, 0, .035);
}
.auth-form-heading { margin-bottom: 26px; }
.auth-form-heading h1 { margin: 0; font-size: 22px; font-weight: 600; }
.auth-form-heading p { margin: 8px 0 0; color: var(--qv-text-muted); font-size: 13px; }
@media (max-width: 900px) { .auth-body { gap: 36px; } .auth-story { margin-top: 40px; } }
@media (max-width: 700px) {
  .auth-shell { padding: 84px 20px 32px; align-items: flex-start; }
  .auth-body { display: block; max-width: 420px; }
  .auth-intro { display: none; }
  .auth-brand { display: flex; justify-content: center; margin: 12px 0 30px; }
  .auth-card { padding: 4px; }
}
</style>
