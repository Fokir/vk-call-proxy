package com.callvpn.app

import android.app.PendingIntent
import android.content.BroadcastReceiver
import android.content.Context
import android.content.Intent
import android.content.IntentFilter
import android.net.VpnService
import android.os.Build
import android.service.quicksettings.Tile
import android.service.quicksettings.TileService
import androidx.core.content.ContextCompat
import androidx.localbroadcastmanager.content.LocalBroadcastManager

class VpnTileService : TileService() {

    private val stateReceiver = object : BroadcastReceiver() {
        override fun onReceive(context: Context?, intent: Intent?) {
            updateTile()
        }
    }

    override fun onStartListening() {
        super.onStartListening()
        LocalBroadcastManager.getInstance(this)
            .registerReceiver(stateReceiver, IntentFilter(CallVpnService.ACTION_STATE_CHANGED))
        updateTile()
    }

    override fun onStopListening() {
        LocalBroadcastManager.getInstance(this)
            .unregisterReceiver(stateReceiver)
        super.onStopListening()
    }

    override fun onClick() {
        super.onClick()

        when (CallVpnService.currentState) {
            "connected", "connecting" -> {
                val intent = Intent(this, CallVpnService::class.java).apply {
                    action = CallVpnService.ACTION_STOP
                }
                startService(intent)
            }
            else -> {
                // Check if VPN permission is granted
                val vpnIntent = VpnService.prepare(this)
                if (vpnIntent != null) {
                    openApp()
                    return
                }

                // Check saved settings
                val prefs = getSharedPreferences("callvpn", Context.MODE_PRIVATE)
                val callLink = prefs.getString("call_link", "") ?: ""
                if (callLink.isBlank()) {
                    openApp()
                    return
                }

                val token = prefs.getString("token", "") ?: ""
                val numConns = prefs.getInt("num_conns", 4)
                val connectionMode = prefs.getString("connection_mode", "Relay") ?: "Relay"
                val serverAddr = if (connectionMode == "Direct") {
                    prefs.getString("server_addr", "") ?: ""
                } else ""

                val intent = Intent(this, CallVpnService::class.java).apply {
                    action = CallVpnService.ACTION_START
                    putExtra(CallVpnService.EXTRA_CALL_LINK, callLink)
                    putExtra(CallVpnService.EXTRA_SERVER_ADDR, serverAddr)
                    putExtra(CallVpnService.EXTRA_NUM_CONNS, numConns)
                    putExtra(CallVpnService.EXTRA_TOKEN, token)
                }
                ContextCompat.startForegroundService(this, intent)
            }
        }
    }

    @Suppress("DEPRECATION")
    private fun openApp() {
        val activityIntent = Intent(this, MainActivity::class.java).apply {
            addFlags(Intent.FLAG_ACTIVITY_NEW_TASK)
        }
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.UPSIDE_DOWN_CAKE) {
            val pending = PendingIntent.getActivity(
                this, 0, activityIntent,
                PendingIntent.FLAG_UPDATE_CURRENT or PendingIntent.FLAG_IMMUTABLE
            )
            startActivityAndCollapse(pending)
        } else {
            startActivityAndCollapse(activityIntent)
        }
    }

    private fun updateTile() {
        val tile = qsTile ?: return
        when (CallVpnService.currentState) {
            "connected" -> {
                tile.state = Tile.STATE_ACTIVE
                tile.subtitle = "Подключён"
            }
            "connecting" -> {
                tile.state = Tile.STATE_ACTIVE
                tile.subtitle = "Подключение..."
            }
            else -> {
                tile.state = Tile.STATE_INACTIVE
                tile.subtitle = null
            }
        }
        tile.label = "CallVPN"
        tile.updateTile()
    }
}
