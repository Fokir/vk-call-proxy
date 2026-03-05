package com.callvpn.app

import android.app.Notification
import android.app.NotificationChannel
import android.app.NotificationManager
import android.app.PendingIntent
import android.content.Intent
import android.net.ConnectivityManager
import android.net.Network
import android.net.NetworkCapabilities
import android.net.NetworkRequest
import android.net.VpnService
import android.os.Build
import android.os.ParcelFileDescriptor
import androidx.core.app.NotificationCompat
import androidx.localbroadcastmanager.content.LocalBroadcastManager
import bind.Tunnel
import bind.TunnelConfig
import java.io.FileInputStream
import java.io.FileOutputStream

class CallVpnService : VpnService() {

    private var tunnel: Tunnel? = null
    private var vpnInterface: ParcelFileDescriptor? = null
    private var networkCallback: ConnectivityManager.NetworkCallback? = null
    @Volatile
    private var running = false
    @Volatile
    private var lastBroadcastState = ""

    override fun onCreate() {
        super.onCreate()
        createNotificationChannel()
    }

    override fun onStartCommand(intent: Intent?, flags: Int, startId: Int): Int {
        when (intent?.action) {
            ACTION_START -> startVpn(intent)
            ACTION_STOP -> stopVpn()
        }
        return START_STICKY
    }

    private fun createNotificationChannel() {
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.O) {
            val channel = NotificationChannel(
                CHANNEL_ID,
                "CallVPN",
                NotificationManager.IMPORTANCE_LOW
            ).apply {
                description = "VPN connection status"
            }
            val manager = getSystemService(NotificationManager::class.java)
            manager.createNotificationChannel(channel)
        }
    }

    private fun buildNotification(text: String): Notification {
        val stopIntent = Intent(this, CallVpnService::class.java).apply {
            action = ACTION_STOP
        }
        val stopPending = PendingIntent.getService(
            this, 0, stopIntent,
            PendingIntent.FLAG_UPDATE_CURRENT or PendingIntent.FLAG_IMMUTABLE
        )

        val openIntent = Intent(this, MainActivity::class.java).apply {
            flags = Intent.FLAG_ACTIVITY_SINGLE_TOP
        }
        val openPending = PendingIntent.getActivity(
            this, 0, openIntent,
            PendingIntent.FLAG_UPDATE_CURRENT or PendingIntent.FLAG_IMMUTABLE
        )

        return NotificationCompat.Builder(this, CHANNEL_ID)
            .setContentTitle("CallVPN")
            .setContentText(text)
            .setSmallIcon(android.R.drawable.ic_lock_lock)
            .setContentIntent(openPending)
            .addAction(android.R.drawable.ic_delete, "Отключить", stopPending)
            .setOngoing(true)
            .build()
    }

    private fun broadcastState(state: String) {
        lastBroadcastState = state
        currentState = state
        val intent = Intent(ACTION_STATE_CHANGED).apply {
            putExtra(EXTRA_STATE, state)
        }
        LocalBroadcastManager.getInstance(this).sendBroadcast(intent)
    }

    private fun startVpn(intent: Intent) {
        val callLink = intent.getStringExtra(EXTRA_CALL_LINK) ?: return
        val serverAddr = intent.getStringExtra(EXTRA_SERVER_ADDR) ?: return
        val numConns = intent.getIntExtra(EXTRA_NUM_CONNS, 4)
        val token = intent.getStringExtra(EXTRA_TOKEN) ?: ""

        running = true
        startForeground(NOTIFICATION_ID, buildNotification("Подключение..."))
        broadcastState("connecting")

        // Run tunnel setup on a background thread to avoid blocking the main
        // thread and, critically, to establish the Go tunnel BEFORE the VPN
        // interface captures all traffic (prevents routing loop).
        Thread {
            // 1. Start Go tunnel FIRST — it needs direct internet access to
            //    reach VK API, WebSocket signaling and TURN servers.
            val config = TunnelConfig().apply {
                this.callLink = callLink
                this.serverAddr = serverAddr
                this.numConns = numConns.toLong()
                this.useTCP = true
                this.token = token
            }

            val t = Tunnel()
            tunnel = t
            try {
                t.start(config)
            } catch (e: Exception) {
                tunnel = null
                broadcastState("disconnected")
                stopForeground(STOP_FOREGROUND_REMOVE)
                return@Thread
            }

            // Check if user cancelled while we were connecting.
            if (!running) {
                t.stop()
                tunnel = null
                broadcastState("disconnected")
                stopForeground(STOP_FOREGROUND_REMOVE)
                return@Thread
            }

            // Check again if cancelled before establishing VPN interface.
            if (!running) {
                t.stop()
                tunnel = null
                broadcastState("disconnected")
                stopForeground(STOP_FOREGROUND_REMOVE)
                return@Thread
            }

            // 2. NOW establish VPN interface — tunnel connections are already
            //    up and won't be affected by the route change.
            val builder = Builder()
                .setSession("CallVPN")
                .addAddress("10.0.0.2", 32)
                .addRoute("0.0.0.0", 0)
                .addAddress("fd00::2", 128)
                .addRoute("::", 0)
                .addDnsServer("8.8.8.8")
                .addDnsServer("8.8.4.4")
                .setMtu(1280)
                .setBlocking(true)

            // Exclude our own app from VPN routing so the Go tunnel's
            // TURN/DTLS connections are never captured by the TUN interface.
            try {
                builder.addDisallowedApplication(packageName)
            } catch (_: Exception) { /* API 21+ */ }

            val vpn = builder.establish()
            if (vpn == null) {
                t.stop()
                tunnel = null
                broadcastState("disconnected")
                stopForeground(STOP_FOREGROUND_REMOVE)
                return@Thread
            }
            vpnInterface = vpn

            broadcastState("connected")

            // Register network change callback for fast reconnect.
            registerNetworkCallback()

            val mgr = getSystemService(NotificationManager::class.java)
            mgr.notify(NOTIFICATION_ID, buildNotification("Подключён"))

            // Log poller: reads logs from Go tunnel, broadcasts to UI,
            // and monitors tunnel connection state for reconnect awareness.
            Thread {
                while (running) {
                    try {
                        // ReadLogs atomically reads and clears the buffer.
                        val logs = tunnel?.readLogs() ?: ""
                        if (logs.isNotEmpty()) {
                            val logIntent = Intent(ACTION_LOG).apply {
                                putExtra(EXTRA_LOG_TEXT, logs)
                            }
                            LocalBroadcastManager.getInstance(this@CallVpnService)
                                .sendBroadcast(logIntent)
                        }

                        // Monitor tunnel connection state for reconnect.
                        val isConnected = tunnel?.isConnected ?: false
                        if (!isConnected && lastBroadcastState == "connected") {
                            broadcastState("connecting")
                            mgr.notify(NOTIFICATION_ID, buildNotification("Переподключение..."))
                        } else if (isConnected && lastBroadcastState == "connecting") {
                            broadcastState("connected")
                            mgr.notify(NOTIFICATION_ID, buildNotification("Подключён"))
                        }

                        // Broadcast connection count.
                        val active = tunnel?.activeConns?.toInt() ?: 0
                        val total = tunnel?.totalConns?.toInt() ?: 0
                        val connIntent = Intent(ACTION_CONN_COUNT).apply {
                            putExtra(EXTRA_ACTIVE_CONNS, active)
                            putExtra(EXTRA_TOTAL_CONNS, total)
                        }
                        LocalBroadcastManager.getInstance(this@CallVpnService)
                            .sendBroadcast(connIntent)

                        Thread.sleep(500)
                    } catch (_: InterruptedException) {
                        break
                    } catch (_: Exception) {
                        // ignore
                    }
                }
            }.start()

            // Read from TUN -> write to tunnel
            Thread {
                val input = FileInputStream(vpn.fileDescriptor)
                val buf = ByteArray(1500)
                while (running) {
                    try {
                        val len = input.read(buf)
                        if (len > 0) {
                            tunnel?.writePacket(buf.copyOf(len))
                        }
                    } catch (e: Exception) {
                        if (running) {
                            Thread.sleep(10)
                            continue
                        } else {
                            break
                        }
                    }
                }
            }.start()

            // Read from tunnel -> write to TUN
            Thread {
                val output = FileOutputStream(vpn.fileDescriptor)
                val buf = ByteArray(1500)
                while (running) {
                    try {
                        val len = tunnel?.readPacket(buf) ?: break
                        if (len > 0) {
                            output.write(buf, 0, len.toInt())
                        }
                    } catch (e: Exception) {
                        if (running) {
                            Thread.sleep(10)
                            continue
                        } else {
                            break
                        }
                    }
                }
            }.start()
        }.start()
    }

    private fun stopVpn() {
        running = false
        unregisterNetworkCallback()
        tunnel?.stop()
        vpnInterface?.close()
        broadcastState("disconnected")
        stopForeground(STOP_FOREGROUND_REMOVE)
        stopSelf()
    }

    override fun onDestroy() {
        stopVpn()
        super.onDestroy()
    }

    private fun registerNetworkCallback() {
        val cm = getSystemService(ConnectivityManager::class.java) ?: return
        val cb = object : ConnectivityManager.NetworkCallback() {
            override fun onAvailable(network: Network) {
                tunnel?.onNetworkChanged()
            }
            override fun onLost(network: Network) {
                tunnel?.onNetworkChanged()
            }
            override fun onCapabilitiesChanged(network: Network, caps: NetworkCapabilities) {
                tunnel?.onNetworkChanged()
            }
        }
        val request = NetworkRequest.Builder()
            .addCapability(NetworkCapabilities.NET_CAPABILITY_INTERNET)
            .build()
        cm.registerNetworkCallback(request, cb)
        networkCallback = cb
    }

    private fun unregisterNetworkCallback() {
        val cb = networkCallback ?: return
        networkCallback = null
        try {
            val cm = getSystemService(ConnectivityManager::class.java)
            cm?.unregisterNetworkCallback(cb)
        } catch (_: Exception) { /* already unregistered */ }
    }

    companion object {
        const val ACTION_START = "com.callvpn.START"
        const val ACTION_STOP = "com.callvpn.STOP"
        const val ACTION_STATE_CHANGED = "com.callvpn.STATE_CHANGED"
        const val ACTION_LOG = "com.callvpn.LOG"
        const val EXTRA_CALL_LINK = "call_link"
        const val EXTRA_SERVER_ADDR = "server_addr"
        const val EXTRA_NUM_CONNS = "num_conns"
        const val EXTRA_TOKEN = "token"
        const val EXTRA_STATE = "state"
        const val EXTRA_LOG_TEXT = "log_text"
        const val ACTION_CONN_COUNT = "com.callvpn.CONN_COUNT"
        const val EXTRA_ACTIVE_CONNS = "active_conns"
        const val EXTRA_TOTAL_CONNS = "total_conns"
        const val CHANNEL_ID = "callvpn_channel"
        const val NOTIFICATION_ID = 1

        @Volatile
        var currentState: String = "disconnected"
            private set
    }
}
