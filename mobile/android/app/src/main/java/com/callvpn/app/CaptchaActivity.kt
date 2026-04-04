package com.callvpn.app

import android.app.Activity
import android.os.Bundle
import android.webkit.*
import android.widget.LinearLayout

/**
 * Full-screen Activity that shows VK captcha in a WebView.
 * Started by CaptchaSolverCallback, returns success_token via a static latch.
 */
class CaptchaActivity : Activity() {

    private lateinit var webView: WebView

    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)

        val redirectUri = intent.getStringExtra(EXTRA_REDIRECT_URI) ?: run {
            resultToken = ""
            latch?.countDown()
            finish()
            return
        }

        webView = WebView(this).apply {
            layoutParams = LinearLayout.LayoutParams(
                LinearLayout.LayoutParams.MATCH_PARENT,
                LinearLayout.LayoutParams.MATCH_PARENT
            )
            settings.javaScriptEnabled = true
            settings.domStorageEnabled = true
            settings.userAgentString = "Mozilla/5.0 (Linux; Android 10) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Mobile Safari/537.36"

            // Intercept XHR responses to catch captchaNotRobot.check success_token.
            webViewClient = object : WebViewClient() {
                override fun onPageFinished(view: WebView?, url: String?) {
                    super.onPageFinished(view, url)
                    // Inject JS to intercept XHR responses containing success_token.
                    view?.evaluateJavascript("""
                        (function() {
                            var origOpen = XMLHttpRequest.prototype.open;
                            var origSend = XMLHttpRequest.prototype.send;
                            XMLHttpRequest.prototype.open = function(method, url) {
                                this._captchaUrl = url;
                                return origOpen.apply(this, arguments);
                            };
                            XMLHttpRequest.prototype.send = function() {
                                var xhr = this;
                                this.addEventListener('load', function() {
                                    if (xhr._captchaUrl && xhr._captchaUrl.indexOf('captchaNotRobot.check') !== -1) {
                                        try {
                                            var resp = JSON.parse(xhr.responseText);
                                            if (resp.response && resp.response.success_token) {
                                                Android.onToken(resp.response.success_token);
                                            }
                                        } catch(e) {}
                                    }
                                });
                                return origSend.apply(this, arguments);
                            };
                        })();
                    """.trimIndent(), null)
                }
            }

            addJavascriptInterface(object {
                @JavascriptInterface
                fun onToken(token: String) {
                    resultToken = token
                    latch?.countDown()
                    runOnUiThread { finish() }
                }
            }, "Android")
        }

        setContentView(webView)
        webView.loadUrl(redirectUri)
    }

    override fun onBackPressed() {
        // User cancelled — return empty token.
        resultToken = ""
        latch?.countDown()
        super.onBackPressed()
    }

    override fun onDestroy() {
        webView.destroy()
        super.onDestroy()
    }

    companion object {
        const val EXTRA_REDIRECT_URI = "redirect_uri"

        // Communication between CaptchaSolverCallback (Go thread) and this Activity.
        @Volatile var resultToken: String = ""
        @Volatile var latch: java.util.concurrent.CountDownLatch? = null
    }
}
