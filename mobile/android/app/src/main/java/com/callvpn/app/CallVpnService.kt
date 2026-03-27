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
    private var rootManager: RootManager? = null
    private var tunInterfaceName: String? = null
    private var tunReaderThread: Thread? = null
    private var tunWriterThread: Thread? = null
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
        val vkTokens = intent.getStringExtra(EXTRA_VK_TOKENS) ?: ""

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
                this.vkTokens = vkTokens
            }

            val t = Tunnel()
            tunnel = t

            // Start stage+log poller BEFORE t.start() so UI sees connection stages.
            val mgr = getSystemService(NotificationManager::class.java)
            Thread {
                while (running) {
                    try {
                        val logs = tunnel?.readLogs() ?: ""
                        if (logs.isNotEmpty()) {
                            val logIntent = Intent(ACTION_LOG).apply {
                                putExtra(EXTRA_LOG_TEXT, logs)
                            }
                            LocalBroadcastManager.getInstance(this@CallVpnService)
                                .sendBroadcast(logIntent)
                        }

                        val stage = tunnel?.stage() ?: ""
                        val stageIntent = Intent(ACTION_STAGE).apply {
                            putExtra(EXTRA_STAGE_TEXT, stage)
                        }
                        LocalBroadcastManager.getInstance(this@CallVpnService)
                            .sendBroadcast(stageIntent)

                        // Update notification with stage during connecting
                        if (lastBroadcastState == "connecting" && stage.isNotEmpty()) {
                            mgr.notify(NOTIFICATION_ID, buildNotification(stage))
                        }

                        val isConnected = tunnel?.isConnected ?: false
                        if (!isConnected && lastBroadcastState == "connected") {
                            broadcastState("connecting")
                            mgr.notify(NOTIFICATION_ID, buildNotification("Переподключение..."))
                        } else if (isConnected && lastBroadcastState == "connecting") {
                            broadcastState("connected")
                            mgr.notify(NOTIFICATION_ID, buildNotification("Подключён"))
                        }

                        val active = tunnel?.activeConns()?.toInt() ?: 0
                        val total = tunnel?.totalConns()?.toInt() ?: 0
                        val connIntent = Intent(ACTION_CONN_COUNT).apply {
                            putExtra(EXTRA_ACTIVE_CONNS, active)
                            putExtra(EXTRA_TOTAL_CONNS, total)
                        }
                        LocalBroadcastManager.getInstance(this@CallVpnService)
                            .sendBroadcast(connIntent)

                        Thread.sleep(300)
                    } catch (_: InterruptedException) {
                        break
                    } catch (_: Exception) { }
                }
            }.start()

            try {
                t.start(config)
            } catch (e: Exception) {
                // Read any Go-side logs that were emitted before the error.
                val goLogs = try { t.readLogs() } catch (_: Exception) { "" }
                tunnel = null
                val errorMsg = e.message ?: e.toString()
                val fullLog = if (goLogs.isNullOrEmpty()) "ERROR: $errorMsg"
                    else "$goLogs\nERROR: $errorMsg"
                val logIntent = Intent(ACTION_LOG).apply {
                    putExtra(EXTRA_LOG_TEXT, fullLog)
                }
                LocalBroadcastManager.getInstance(this@CallVpnService)
                    .sendBroadcast(logIntent)
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

            // Exclude apps selected by the user from VPN routing.
            val excludedApps = ExcludedAppsManager(this@CallVpnService).getExcludedPackages()
            for (pkg in excludedApps) {
                try {
                    builder.addDisallowedApplication(pkg)
                } catch (_: Exception) { /* package not installed, skip */ }
            }

            val vpn = builder.establish()
            if (vpn == null) {
                t.stop()
                tunnel = null
                broadcastState("disconnected")
                stopForeground(STOP_FOREGROUND_REMOVE)
                return@Thread
            }
            vpnInterface = vpn

            // Set up hotspot routing through VPN if enabled (requires root).
            val tunName = detectTunName()
            tunInterfaceName = tunName
            val rm = RootManager(this@CallVpnService)
            rootManager = rm
            if (tunName != null && rm.hotspotRoutingEnabled) {
                val ok = rm.setupHotspotRouting(tunName)
                val logMsg = if (ok) "Hotspot routing: enabled (TTL=64)"
                             else "Hotspot routing: FAILED to apply iptables rules"
                val logIntent = Intent(ACTION_LOG).apply {
                    putExtra(EXTRA_LOG_TEXT, logMsg)
                }
                LocalBroadcastManager.getInstance(this@CallVpnService)
                    .sendBroadcast(logIntent)
            }

            broadcastState("connected")

            // Register network change callback for fast reconnect.
            registerNetworkCallback()

            mgr.notify(NOTIFICATION_ID, buildNotification("Подключён"))

            // Read from TUN -> write to tunnel
            tunReaderThread = Thread {
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
            }.also { it.start() }

            // Read from tunnel -> write to TUN
            tunWriterThread = Thread {
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
            }.also { it.start() }
        }.start()
    }

    private fun stopVpn() {
        running = false
        unregisterNetworkCallback()

        // Clean up hotspot routing BEFORE closing TUN interface.
        val tunName = tunInterfaceName
        if (tunName != null) {
            rootManager?.cleanupHotspotRouting(tunName)
        }
        tunInterfaceName = null
        rootManager = null

        // Close VPN interface FIRST to unblock reader/writer threads that
        // may be stuck on blocking I/O.  Without this the threads keep the
        // TUN device alive and Android leaves stale routes in place, which
        // causes "no internet" after disconnect until WiFi is toggled.
        vpnInterface?.close()
        vpnInterface = null

        // Wait for I/O threads to exit (they will get an IOException from
        // the closed FD and break out of their loops).
        try { tunReaderThread?.join(2000) } catch (_: Exception) {}
        try { tunWriterThread?.join(2000) } catch (_: Exception) {}
        tunReaderThread = null
        tunWriterThread = null

        tunnel?.stop()
        broadcastState("disconnected")
        stopForeground(STOP_FOREGROUND_REMOVE)
        stopSelf()
    }

    /** Detects the VPN TUN interface name by scanning /sys/class/net/ */
    private fun detectTunName(): String? {
        val tunRegex = Regex("^tun\\d+$")
        val netDir = java.io.File("/sys/class/net/")
        return netDir.listFiles()
            ?.map { it.name }
            ?.firstOrNull { tunRegex.matches(it) }
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
            // Note: onCapabilitiesChanged intentionally omitted — it fires
            // too frequently (signal strength changes, etc.) and the Go side
            // debounces onAvailable/onLost which are sufficient for detecting
            // real network switches (WiFi↔cellular).
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
        const val EXTRA_VK_TOKENS = "vk_tokens"
        const val EXTRA_STATE = "state"
        const val EXTRA_LOG_TEXT = "log_text"
        const val ACTION_CONN_COUNT = "com.callvpn.CONN_COUNT"
        const val EXTRA_ACTIVE_CONNS = "active_conns"
        const val EXTRA_TOTAL_CONNS = "total_conns"
        const val ACTION_STAGE = "com.callvpn.STAGE"
        const val EXTRA_STAGE_TEXT = "stage_text"
        const val CHANNEL_ID = "callvpn_channel"
        const val NOTIFICATION_ID = 1

        @Volatile
        var currentState: String = "disconnected"
            private set
    }
}
