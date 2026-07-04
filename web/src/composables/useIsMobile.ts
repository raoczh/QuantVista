import { ref, onMounted, onUnmounted } from 'vue'

// 移动端断点（与全站 CSS @media (max-width: 768px) 保持一致）。
const QUERY = '(max-width: 768px)'

/**
 * 响应式移动端判定：用于模板里切换 Naive 组件的布局 props
 * （如表单 label-placement left→top）。纯样式收窄优先用 CSS 媒体查询，别用这个。
 */
export function useIsMobile() {
  const mql = window.matchMedia(QUERY)
  const isMobile = ref(mql.matches)
  const onChange = (e: MediaQueryListEvent) => {
    isMobile.value = e.matches
  }
  onMounted(() => mql.addEventListener('change', onChange))
  onUnmounted(() => mql.removeEventListener('change', onChange))
  return { isMobile }
}
