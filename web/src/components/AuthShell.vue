<script setup lang="ts">
import { computed, h } from 'vue'
import { NCard, NDropdown, NButton, NIcon } from 'naive-ui'
import { storeToRefs } from 'pinia'
import { useThemeStore } from '@/stores/theme'
import { useUi } from '@/composables/useUi'
import BrandLogo from '@/components/BrandLogo.vue'

withDefaults(defineProps<{ subtitle?: string }>(), { subtitle: 'AI 股票研究平台' })

const themeStore = useThemeStore()
const { currentKey, preset } = storeToRefs(themeStore)
const { vars, primaryAlpha } = useUi()

// 主题感知的柔光渐变背景：顶部一抹主色光晕 + 主题 body 底色。
const bgStyle = computed(() => ({
  background: `radial-gradient(1100px 560px at 50% -12%, ${primaryAlpha(0.2)}, transparent 70%), ${vars.value.bodyColor}`,
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

    <div class="auth-body">
      <div class="auth-brand">
        <BrandLogo :size="44" />
        <p class="auth-subtitle">{{ subtitle }}</p>
      </div>
      <n-card class="auth-card">
        <slot />
      </n-card>
    </div>
  </div>
</template>

<style scoped>
.auth-shell {
  position: relative;
  min-height: 100vh;
  display: flex;
  align-items: center;
  justify-content: center;
  padding: 24px;
}
.auth-topbar {
  position: absolute;
  top: 16px;
  right: 20px;
}
.auth-body {
  width: 100%;
  max-width: 400px;
  display: flex;
  flex-direction: column;
}
.auth-brand {
  display: flex;
  flex-direction: column;
  align-items: center;
  gap: 12px;
  margin-bottom: 22px;
}
.auth-subtitle {
  margin: 0;
  font-size: 13px;
  opacity: 0.6;
}
.auth-card {
  border-radius: 16px;
  box-shadow: 0 12px 40px rgba(0, 0, 0, 0.12);
}
</style>
