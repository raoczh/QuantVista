# 图标与启动图

当前使用 Capacitor 模板默认图标。要换成正式品牌图标：

1. 在本目录放置源图：
   - `icon.png` —— 1024×1024，App 图标；
   - `splash.png` —— 2732×2732，启动图（居中 logo，四周留足裁切余量）。
2. 在 `mobile/` 下执行一次性生成（工具无需常驻依赖）：

   ```bash
   npx @capacitor/assets generate --android
   ```

   会自动写入 `android/app/src/main/res/` 各密度目录（mipmap/drawable）。
3. 重新构建 APK 生效。

源图（icon.png/splash.png）可入库；生成的 res 产物也随 android/ 工程入库。
