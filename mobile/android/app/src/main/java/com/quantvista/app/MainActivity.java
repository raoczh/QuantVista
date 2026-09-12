package com.quantvista.app;

import android.net.Uri;
import android.os.Bundle;
import android.webkit.WebResourceRequest;
import android.webkit.WebView;
import com.getcapacitor.Bridge;
import com.getcapacitor.BridgeActivity;
import com.getcapacitor.BridgeWebViewClient;

public class MainActivity extends BridgeActivity {
    @Override
    public void onCreate(Bundle savedInstanceState) {
        super.onCreate(savedInstanceState);
        Bridge bridge = getBridge();
        if (bridge == null) return;

        // errorPath 是本地页面，reload 只会再次打开错误页。由原生层按当前
        // server.url 重试，避免把部署域名重复硬编码到 HTML 中。
        bridge.setWebViewClient(new BridgeWebViewClient(bridge) {
            @Override
            public boolean shouldOverrideUrlLoading(WebView view, WebResourceRequest request) {
                String errorUrl = bridge.getErrorUrl();
                String serverUrl = bridge.getServerUrl();
                if (errorUrl != null && serverUrl != null && request.isForMainFrame()
                        && errorUrl.equals(view.getUrl())) {
                    Uri retryUrl = Uri.parse(errorUrl).buildUpon()
                            .path("/__quantvista_retry__").clearQuery().fragment(null).build();
                    if (retryUrl.equals(request.getUrl())) {
                        view.loadUrl(serverUrl);
                        return true;
                    }
                }
                return super.shouldOverrideUrlLoading(view, request);
            }
        });
    }
}
