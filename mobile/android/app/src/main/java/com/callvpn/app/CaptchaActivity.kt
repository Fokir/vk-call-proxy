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

            webViewClient = object : WebViewClient() {
                override fun onPageFinished(view: WebView?, url: String?) {
                    super.onPageFinished(view, url)
                    android.util.Log.d("CaptchaActivity", "onPageFinished: $url")
                    // Start polling for success_token in the DOM after page loads.
                    startTokenPolling(view)
                    // Inject JS to intercept both XHR and fetch responses.
                    view?.evaluateJavascript("""
                        (function() {
                            function checkResponse(url, text) {
                                if (url && url.indexOf('captchaNotRobot.check') !== -1) {
                                    try {
                                        var resp = JSON.parse(text);
                                        if (resp.response && resp.response.success_token) {
                                            Android.onToken(resp.response.success_token);
                                        }
                                    } catch(e) {}
                                }
                            }
                            // Intercept XMLHttpRequest
                            var origOpen = XMLHttpRequest.prototype.open;
                            var origSend = XMLHttpRequest.prototype.send;
                            XMLHttpRequest.prototype.open = function(method, url) {
                                this._captchaUrl = url;
                                return origOpen.apply(this, arguments);
                            };
                            XMLHttpRequest.prototype.send = function() {
                                var xhr = this;
                                this.addEventListener('load', function() {
                                    checkResponse(xhr._captchaUrl, xhr.responseText);
                                });
                                return origSend.apply(this, arguments);
                            };
                            // Intercept fetch
                            var origFetch = window.fetch;
                            window.fetch = function(input, init) {
                                var url = typeof input === 'string' ? input : (input && input.url ? input.url : '');
                                return origFetch.apply(this, arguments).then(function(response) {
                                    if (url.indexOf('captchaNotRobot.check') !== -1) {
                                        response.clone().text().then(function(text) {
                                            checkResponse(url, text);
                                        });
                                    }
                                    return response;
                                });
                            };
                        })();
                    """.trimIndent(), null)
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
                    resultToken = token
                    latch?.countDown()
                    runOnUiThread { finish() }
                }
            }, "Android")
        }

        setContentView(webView)
        webView.loadUrl(redirectUri)
    }

    private var polling = false

    private fun startTokenPolling(view: WebView?) {
        if (polling || view == null) return
        polling = true
        // Poll every 500ms: check if captcha was solved by looking for
        // the checked checkbox state or by re-injecting XHR/fetch intercept.
        val handler = android.os.Handler(mainLooper)
        val runnable = object : Runnable {
            override fun run() {
                if (!polling) return
                // Try to read success_token from VK captcha SDK's internal state.
                view.evaluateJavascript("""
                    (function() {
                        // Method 1: Check if checkbox has 'checked' class (captcha passed)
                        var cb = document.querySelector('input[type="checkbox"]');
                        if (cb && cb.checked) {
                            // Captcha was checked, now look for success indicator
                            var ok = document.querySelector('[class*="checkboxBlock--success"], [class*="Success"], [class*="success"]');
                            if (ok) return 'CHECKED_SUCCESS';
                        }
                        return '';
                    })()
                """.trimIndent()) { result ->
                    android.util.Log.d("CaptchaActivity", "poll result: $result")
                }
                handler.postDelayed(this, 500)
            }
        }
        handler.postDelayed(runnable, 1000)
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
