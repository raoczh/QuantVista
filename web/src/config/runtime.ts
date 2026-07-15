// 运行环境判断：Android 壳（Capacitor）会在 WebView 里注入 window.Capacitor 桥。
// 硬约束：所有 @capacitor/* 调用必须动态 import 且以 isNativeApp 守卫——
// 浏览器端不加载对应 chunk、web 包不增重（见 docs/ANDROID_APP_PLAN.md §4.3）。
interface CapacitorGlobal {
  isNativePlatform?: () => boolean
}

export const isNativeApp: boolean =
  typeof window !== 'undefined' &&
  Boolean((window as { Capacitor?: CapacitorGlobal }).Capacitor?.isNativePlatform?.())
