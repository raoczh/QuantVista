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
  <PageContainer>
    <div class="placeholder">
      <n-result
        status="info"
        :title="title"
        description="该模块尚未实现，将随实施路线图阶段推进逐步上线（见 docs/ROADMAP.md）。"
      >
        <template #footer>
          <n-button @click="router.push('/')">返回市场首页</n-button>
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
