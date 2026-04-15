package com.callvpn.app

import android.app.Activity
import android.os.Bundle
import android.os.Handler
import android.os.Looper
import android.webkit.*
import android.widget.LinearLayout

/**
 * Full-screen Activity that shows VK captcha in a WebView.
 * Started by CaptchaSolverCallback, returns success_token via a static latch.
 *
 * The VK captcha widget (id.vk.com/not_robot_captcha) runs entirely in the top
 * frame — the captchaNotRobot.check fetch is made from id.vk.com JavaScript
 * directly, not from a cross-origin iframe — so patching window.fetch and
 * XMLHttpRequest from the top-frame context is sufficient to capture the
 * success_token.
 */
class CaptchaActivity : Activity() {

    private var webView: WebView? = null
    private val mainHandler = Handler(Looper.getMainLooper())
    @Volatile private var delivered = false

    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)

        val redirectUri = intent.getStringExtra(EXTRA_REDIRECT_URI) ?: run {
            deliverToken("")
            return
        }

        val wv = WebView(this).apply {
            layoutParams = LinearLayout.LayoutParams(
                LinearLayout.LayoutParams.MATCH_PARENT,
                LinearLayout.LayoutParams.MATCH_PARENT
            )
            settings.javaScriptEnabled = true
            settings.domStorageEnabled = true
            settings.userAgentString = "Mozilla/5.0 (Linux; Android 10) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Mobile Safari/537.36"

            webViewClient = object : WebViewClient() {
                override fun onPageStarted(view: WebView?, url: String?, favicon: android.graphics.Bitmap?) {
                    super.onPageStarted(view, url, favicon)
                    // Inject as early as possible so the hook is in place
                    // before the captcha SDK binds its handlers.
                    view?.evaluateJavascript(INTERCEPT_JS, null)
                }

                override fun onPageFinished(view: WebView?, url: String?) {
                    super.onPageFinished(view, url)
                    // Re-inject in case the page replaced window.fetch after
                    // onPageStarted (some SDKs do this during their own init).
                    view?.evaluateJavascript(INTERCEPT_JS, null)
                }
            }

            webChromeClient = object : WebChromeClient() {
                override fun onConsoleMessage(consoleMessage: ConsoleMessage?): Boolean {
                    android.util.Log.d("CaptchaActivity", "JS: ${consoleMessage?.message()}")
                    return true
                }
            }

            addJavascriptInterface(object {
                @JavascriptInterface
                fun onToken(token: String) {
                    deliverToken(token)
                }
            }, "Android")
        }

        webView = wv
        setContentView(wv)
        wv.loadUrl(redirectUri)
    }

    private fun deliverToken(token: String) {
        if (delivered) return
        delivered = true
        resultToken = token
        latch?.countDown()
        mainHandler.post {
            if (!isFinishing) finish()
        }
    }

    override fun onBackPressed() {
        deliverToken("")
        super.onBackPressed()
    }

    override fun onDestroy() {
        mainHandler.removeCallbacksAndMessages(null)
        // Ensure the Go thread unblocks even if the user dismissed the
        // Activity without solving — otherwise the latch waits 2 full minutes.
        if (!delivered) {
            delivered = true
            resultToken = ""
            latch?.countDown()
        }
        webView?.let { wv ->
            wv.stopLoading()
            wv.webViewClient = WebViewClient()
            wv.webChromeClient = null
            (wv.parent as? android.view.ViewGroup)?.removeView(wv)
            wv.destroy()
        }
        webView = null
        super.onDestroy()
    }

    companion object {
        const val EXTRA_REDIRECT_URI = "redirect_uri"

        // Communication between CaptchaSolverCallback (Go thread) and this Activity.
        @Volatile var resultToken: String = ""
        @Volatile var latch: java.util.concurrent.CountDownLatch? = null

        private const val INTERCEPT_JS = """
            (function() {
                if (window.__captchaHookInstalled) return;
                window.__captchaHookInstalled = true;

                function reportToken(text) {
                    try {
                        var resp = JSON.parse(text);
                        var token = resp && resp.response && resp.response.success_token;
                        if (token && window.Android && window.Android.onToken) {
                            window.Android.onToken(token);
                        }
                    } catch (e) {}
                }

                var origOpen = XMLHttpRequest.prototype.open;
                var origSend = XMLHttpRequest.prototype.send;
                XMLHttpRequest.prototype.open = function(method, url) {
                    this.__captchaUrl = url || '';
                    return origOpen.apply(this, arguments);
                };
                XMLHttpRequest.prototype.send = function() {
                    var xhr = this;
                    this.addEventListener('load', function() {
                        if (xhr.__captchaUrl && xhr.__captchaUrl.indexOf('captchaNotRobot.check') !== -1) {
                            reportToken(xhr.responseText);
                        }
                    });
                    return origSend.apply(this, arguments);
                };

                if (window.fetch) {
                    var origFetch = window.fetch;
                    window.fetch = function(input, init) {
                        var url = typeof input === 'string'
                            ? input
                            : (input && input.url ? input.url : '');
                        var p = origFetch.apply(this, arguments);
                        if (url && url.indexOf('captchaNotRobot.check') !== -1) {
                            p.then(function(resp) {
                                resp.clone().text().then(reportToken).catch(function(){});
                            }).catch(function(){});
                        }
                        return p;
                    };
                }
            })();
        """
    }
}
