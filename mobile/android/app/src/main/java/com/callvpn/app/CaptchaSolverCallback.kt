package com.callvpn.app

import android.content.Context
import android.content.Intent
import bind.CaptchaCallback
import java.util.concurrent.CountDownLatch
import java.util.concurrent.TimeUnit

/**
 * Implements Go's bind.CaptchaCallback interface.
 * When VK requires captcha, Go calls showCaptcha() which launches CaptchaActivity
 * with a WebView and blocks until the user solves it (or timeout).
 */
class CaptchaSolverCallback(private val context: Context) : CaptchaCallback {

    override fun showCaptcha(redirectURI: String): String {
        val latch = CountDownLatch(1)
        CaptchaActivity.latch = latch
        CaptchaActivity.resultToken = ""

        val intent = Intent(context, CaptchaActivity::class.java).apply {
            putExtra(CaptchaActivity.EXTRA_REDIRECT_URI, redirectURI)
            addFlags(Intent.FLAG_ACTIVITY_NEW_TASK)
        }
        context.startActivity(intent)

        // Block Go thread until WebView returns token or 2 min timeout.
        latch.await(2, TimeUnit.MINUTES)
        return CaptchaActivity.resultToken
    }
}
