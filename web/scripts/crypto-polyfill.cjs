// Node 16 兼容垫片：新版 vite 在 resolveConfig 里调用 node:crypto 顶层的 getRandomValues，
// 该导出 Node 17+ 才有；Node 16 的实现在 crypto.webcrypto 上。这里把它补到 crypto 模块与全局，
// 仅用于本地构建验证，不进产物。
const nodeCrypto = require('crypto')
const wc = nodeCrypto.webcrypto
try {
  if (typeof nodeCrypto.getRandomValues !== 'function') {
    const grv = wc && typeof wc.getRandomValues === 'function'
      ? wc.getRandomValues.bind(wc)
      : (arr) => nodeCrypto.randomFillSync(arr)
    nodeCrypto.getRandomValues = grv
    if (!globalThis.crypto || typeof globalThis.crypto.getRandomValues !== 'function') {
      Object.defineProperty(globalThis, 'crypto', {
        value: wc || { getRandomValues: grv },
        configurable: true,
        writable: true,
      })
    }
  }
} catch (e) {
  // 尽力而为
}
