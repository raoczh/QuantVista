<script setup lang="ts">
import { computed } from 'vue'
import { useRoute } from 'vue-router'

const props = defineProps<{
  searchActive: boolean
  todoBadge: string | null
  todoIncomplete: boolean
}>()
const emit = defineEmits<{ (e: 'open-search'): void }>()

type MobileRouteName = 'home' | 'watchlist' | 'today' | 'positions'

const route = useRoute()
const todayAriaLabel = computed(() => {
  if (!props.todoBadge) return '今日'
  if (props.todoIncomplete) return `今日，${props.todoBadge}，待办清单读取不完整`
  return `今日，${props.todoBadge} 项待办`
})

function isRouteActive(name: MobileRouteName) {
  return !props.searchActive && route.name === name
}
</script>

<template>
  <nav class="mobile-bottom-nav" aria-label="移动端主导航">
    <RouterLink
      to="/"
      class="mobile-nav-item"
      :class="{ 'is-active': isRouteActive('home') }"
      :aria-current="isRouteActive('home') ? 'page' : undefined"
    >
      <svg class="mobile-nav-icon" viewBox="0 0 24 24" fill="none" stroke="currentColor" aria-hidden="true">
        <path d="m3 11 9-8 9 8" />
        <path d="M5 10v10h14V10" />
        <path d="M9 20v-6h6v6" />
      </svg>
      <span class="mobile-nav-label">首页</span>
    </RouterLink>

    <RouterLink
      to="/watchlist"
      class="mobile-nav-item"
      :class="{ 'is-active': isRouteActive('watchlist') }"
      :aria-current="isRouteActive('watchlist') ? 'page' : undefined"
    >
      <svg class="mobile-nav-icon" viewBox="0 0 24 24" fill="none" stroke="currentColor" aria-hidden="true">
        <path d="m12 3 2.8 5.7 6.2.9-4.5 4.4 1.1 6.2-5.6-3-5.6 3 1.1-6.2-4.5-4.4 6.2-.9L12 3Z" />
      </svg>
      <span class="mobile-nav-label">自选</span>
    </RouterLink>

    <button
      class="mobile-nav-item"
      :class="{ 'is-active': searchActive }"
      type="button"
      :aria-pressed="searchActive"
      aria-label="搜股票"
      @click="emit('open-search')"
    >
      <svg class="mobile-nav-icon" viewBox="0 0 24 24" fill="none" stroke="currentColor" aria-hidden="true">
        <circle cx="11" cy="11" r="7" />
        <path d="m20 20-3.5-3.5" />
      </svg>
      <span class="mobile-nav-label">搜索</span>
    </button>

    <RouterLink
      to="/today"
      class="mobile-nav-item"
      :class="{ 'is-active': isRouteActive('today') }"
      :aria-current="isRouteActive('today') ? 'page' : undefined"
      :aria-label="todayAriaLabel"
    >
      <span class="mobile-nav-icon-wrap">
        <svg class="mobile-nav-icon" viewBox="0 0 24 24" fill="none" stroke="currentColor" aria-hidden="true">
          <path d="M7 3v4M17 3v4M4 10h16" />
          <rect x="4" y="5" width="16" height="16" rx="2" />
          <path d="m8 15 2.5 2.5L16 12" />
        </svg>
        <span
          v-if="todoBadge"
          class="mobile-nav-badge"
          :class="{ 'is-incomplete': todoIncomplete }"
          aria-hidden="true"
        >
          {{ todoBadge }}
        </span>
      </span>
      <span class="mobile-nav-label">今日</span>
    </RouterLink>

    <RouterLink
      to="/positions"
      class="mobile-nav-item"
      :class="{ 'is-active': isRouteActive('positions') }"
      :aria-current="isRouteActive('positions') ? 'page' : undefined"
    >
      <svg class="mobile-nav-icon" viewBox="0 0 24 24" fill="none" stroke="currentColor" aria-hidden="true">
        <rect x="3" y="7" width="18" height="13" rx="2" />
        <path d="M8 7V5a2 2 0 0 1 2-2h4a2 2 0 0 1 2 2v2M3 12h18M10 12v2h4v-2" />
      </svg>
      <span class="mobile-nav-label">持仓</span>
    </RouterLink>
  </nav>
</template>

<style scoped>
.mobile-bottom-nav {
  position: fixed;
  right: 0;
  bottom: 0;
  left: 0;
  z-index: 90;
  display: none;
  grid-template-columns: repeat(5, minmax(0, 1fr));
  box-sizing: border-box;
  min-height: calc(56px + env(safe-area-inset-bottom, 0px));
  padding-bottom: env(safe-area-inset-bottom, 0px);
  border-top: 1px solid var(--qv-header-border);
  background: var(--qv-header-bg);
  backdrop-filter: blur(16px) saturate(1.5);
  -webkit-backdrop-filter: blur(16px) saturate(1.5);
}

.mobile-nav-item {
  position: relative;
  display: flex;
  min-width: 0;
  min-height: 56px;
  align-items: center;
  justify-content: center;
  flex-direction: column;
  gap: 2px;
  box-sizing: border-box;
  padding: 5px 2px 4px;
  border: 0;
  background: transparent;
  color: inherit;
  font: inherit;
  text-decoration: none;
  cursor: pointer;
  -webkit-tap-highlight-color: transparent;
}

.mobile-nav-item::before {
  position: absolute;
  top: -1px;
  left: 50%;
  width: 20px;
  height: 3px;
  border-radius: 0 0 3px 3px;
  background: transparent;
  content: '';
  transform: translateX(-50%);
}

.mobile-nav-item.is-active {
  color: var(--qv-menu-active-text);
}

.mobile-nav-item.is-active::before {
  background: var(--qv-menu-active-text);
}

.mobile-nav-item:focus-visible {
  outline: 2px solid var(--qv-menu-active-text);
  outline-offset: -3px;
}

.mobile-nav-icon {
  width: 20px;
  height: 20px;
  flex: 0 0 20px;
  stroke-width: 2;
  stroke-linecap: round;
  stroke-linejoin: round;
}

.mobile-nav-icon-wrap {
  position: relative;
  display: block;
  width: 20px;
  height: 20px;
  flex: 0 0 20px;
}

.mobile-nav-badge {
  position: absolute;
  top: -6px;
  left: 14px;
  min-width: 14px;
  height: 14px;
  box-sizing: border-box;
  padding: 0 3px;
  border-radius: 7px;
  background: var(--qv-badge-bg);
  color: #fff;
  font-size: 9px;
  font-weight: 700;
  line-height: 14px;
  text-align: center;
  white-space: nowrap;
}

.mobile-nav-badge.is-incomplete {
  background: var(--qv-badge-incomplete-bg);
}

.mobile-nav-label {
  max-width: 100%;
  overflow: hidden;
  font-size: 11px;
  font-weight: 500;
  line-height: 15px;
  letter-spacing: 0;
  text-overflow: ellipsis;
  white-space: nowrap;
}

@media (max-width: 768px) {
  .mobile-bottom-nav {
    display: grid;
  }
}
</style>
