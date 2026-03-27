package com.callvpn.app

import android.content.BroadcastReceiver
import android.content.Context
import android.content.Intent
import android.net.VpnService
import androidx.core.content.ContextCompat

/**
 * ADB-controllable receiver for automated testing.
 *
 * Connect:  adb shell am broadcast -a com.callvpn.DEBUG_CONNECT com.callvpn.app
 * Disconnect: adb shell am broadcast -a com.callvpn.DEBUG_DISCONNECT com.callvpn.app
 */
class DebugReceiver : BroadcastReceiver() {
    override fun onReceive(context: Context, intent: Intent?) {
        when (intent?.action) {
            "com.callvpn.DEBUG_CONNECT" -> {
                // Only works if VPN permission was already granted
                val vpnIntent = VpnService.prepare(context)
                if (vpnIntent != null) {
                    // VPN permission not granted yet — can't start from receiver
                    return
                }
                val profileManager = ProfileManager(context)
                val profile = profileManager.getActiveProfile() ?: return
                val callLink = parseCallLink(profile.callLink)
                if (callLink.isBlank()) return

                val serverAddr = if (profile.connectionMode == "direct") profile.serverAddr else ""
                val svcIntent = Intent(context, CallVpnService::class.java).apply {
                    action = CallVpnService.ACTION_START
                    putExtra(CallVpnService.EXTRA_CALL_LINK, callLink)
                    putExtra(CallVpnService.EXTRA_SERVER_ADDR, serverAddr)
                    putExtra(CallVpnService.EXTRA_NUM_CONNS, profile.numConns)
                    putExtra(CallVpnService.EXTRA_TOKEN, profile.token)
                    putExtra(CallVpnService.EXTRA_VK_TOKENS, profile.vkTokens)
                }
                ContextCompat.startForegroundService(context, svcIntent)
            }
            "com.callvpn.DEBUG_DISCONNECT" -> {
                val svcIntent = Intent(context, CallVpnService::class.java).apply {
                    action = CallVpnService.ACTION_STOP
                }
                context.startService(svcIntent)
            }
        }
    }
}

private fun parseCallLink(input: String): String {
    val vkRegex = Regex("""vk\.com/call/join/([A-Za-z0-9_-]+)""")
    val vkMatch = vkRegex.find(input)
    if (vkMatch != null) return vkMatch.groupValues[1]
    val telemostRegex = Regex("""telemost\.yandex\.\w+/j/(\d+)""")
    val telemostMatch = telemostRegex.find(input)
    if (telemostMatch != null) return telemostMatch.groupValues[1]
    return input.trim()
}
