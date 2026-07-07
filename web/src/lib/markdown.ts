// Markdown 安全渲染（S1 流式问答配套）：marked 转 HTML + DOMPurify 消毒防 XSS。
// LLM 输出属不可信内容，任何 v-html 渲染前必须过这里——别在组件里直接 marked.parse。
import { marked } from 'marked'
import DOMPurify from 'dompurify'

marked.setOptions({
  breaks: true, // 单换行即 <br>（对话式输出的自然习惯）
  gfm: true,
  async: false,
})

// 白名单从紧：问答提示词已要求不输出表格/图片/链接，这里同口径兜底
//（链接一并去掉——LLM 编造的 URL 点出去是钓鱼面）。
const purifyConfig = {
  ALLOWED_TAGS: [
    'p', 'br', 'strong', 'b', 'em', 'i', 'del', 's', 'code', 'pre',
    'ul', 'ol', 'li', 'blockquote', 'h1', 'h2', 'h3', 'h4', 'hr', 'span',
  ],
  ALLOWED_ATTR: [] as string[],
}

export function renderMarkdown(text: string): string {
  if (!text) return ''
  const html = marked.parse(text) as string
  return DOMPurify.sanitize(html, purifyConfig)
}
