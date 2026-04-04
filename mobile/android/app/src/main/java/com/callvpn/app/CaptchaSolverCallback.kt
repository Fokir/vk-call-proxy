package com.callvpn.app

import android.app.NotificationChannel
import android.app.NotificationManager
import android.app.PendingIntent
import android.content.Context
import android.content.Intent
import android.os.Build
import androidx.core.app.NotificationCompat
import bind.CaptchaCallback
import java.util.concurrent.CountDownLatch
import java.util.concurrent.TimeUnit

/**
 * Implements Go's bind.CaptchaCallback interface.
 * When VK requires captcha:
 * - If app is in foreground: directly opens CaptchaActivity
 * - If app is in background: shows a high-priority notification that opens CaptchaActivity on tap
 * Go thread blocks until the captcha is solved or times out (2 min).
 */
class CaptchaSolverCallback(private val context: Context) : CaptchaCallback {

    override fun showCaptcha(redirectURI: String): String {
        val latch = CountDownLatch(1)
        CaptchaActivity.latch = latch
        CaptchaActivity.resultToken = ""

        val activityIntent = Intent(context, CaptchaActivity::class.java).apply {
            putExtra(CaptchaActivity.EXTRA_REDIRECT_URI, redirectURI)
            addFlags(Intent.FLAG_ACTIVITY_NEW_TASK or Intent.FLAG_ACTIVITY_CLEAR_TOP)
        }

        // Try to launch Activity directly first.
        try {
            context.startActivity(activityIntent)
        } catch (_: Exception) {
            // Background launch blocked on Android 10+ — fall through to notification.
        }

        // Always show notification as backup (user might not see the Activity).
        showCaptchaNotification(activityIntent)

        // Block Go thread until WebView returns token or 2 min timeout.
        latch.await(2, TimeUnit.MINUTES)

        // Dismiss notification after captcha is solved/cancelled.
        dismissCaptchaNotification()

        return CaptchaActivity.resultToken
    }

    private fun showCaptchaNotification(activityIntent: Intent) {
        ensureNotificationChannel()

        val pendingIntent = PendingIntent.getActivity(
            context, CAPTCHA_REQUEST_CODE, activityIntent,
            PendingIntent.FLAG_UPDATE_CURRENT or PendingIntent.FLAG_IMMUTABLE
        )

        val notification = NotificationCompat.Builder(context, CAPTCHA_CHANNEL_ID)
            .setSmallIcon(android.R.drawable.ic_dialog_alert)
            .setContentTitle("Требуется подтверждение")
            .setContentText("Нажмите чтобы пройти проверку VK")
            .setPriority(NotificationCompat.PRIORITY_HIGH)
            .setCategory(NotificationCompat.CATEGORY_ALARM)
            .setAutoCancel(true)
            .setContentIntent(pendingIntent)
            .setDefaults(NotificationCompat.DEFAULT_ALL)
            .build()

        val manager = context.getSystemService(Context.NOTIFICATION_SERVICE) as NotificationManager
        manager.notify(CAPTCHA_NOTIFICATION_ID, notification)
    }

    private fun dismissCaptchaNotification() {
        val manager = context.getSystemService(Context.NOTIFICATION_SERVICE) as NotificationManager
        manager.cancel(CAPTCHA_NOTIFICATION_ID)
    }

    private fun ensureNotificationChannel() {
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.O) {
            val channel = NotificationChannel(
                CAPTCHA_CHANNEL_ID,
                "Captcha",
                NotificationManager.IMPORTANCE_HIGH
            ).apply {
                description = "VK captcha verification required"
                enableVibration(true)
            }
            val manager = context.getSystemService(Context.NOTIFICATION_SERVICE) as NotificationManager
            manager.createNotificationChannel(channel)
        }
    }

    companion object {
        private const val CAPTCHA_CHANNEL_ID = "callvpn_captcha"
        private const val CAPTCHA_NOTIFICATION_ID = 42
        private const val CAPTCHA_REQUEST_CODE = 42
    }
}
