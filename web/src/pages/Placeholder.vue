<script setup lang="ts">
import { computed } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { NResult, NButton } from 'naive-ui'
import PageContainer from '@/components/PageContainer.vue'

const route = useRoute()
const router = useRouter()
const title = computed(() => (route.meta.title as string) || '页面')
</script>

<template>
  <PageContainer :title="title">
    <div class="placeholder">
      <n-result
        status="404"
        description="页面不存在，地址可能已更改。可以返回首页继续查看。"
      >
        <template #footer>
          <n-button type="primary" @click="router.push('/')">返回今日概览</n-button>
        </template>
      </n-result>
    </div>
  </PageContainer>
</template>

<style scoped>
.placeholder {
  display: flex;
  align-items: center;
  justify-content: center;
  min-height: 60vh;
}
</style>
