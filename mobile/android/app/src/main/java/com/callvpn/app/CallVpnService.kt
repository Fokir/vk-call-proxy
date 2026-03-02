package com.callvpn.app

import android.app.Notification
import android.app.NotificationChannel
import android.app.NotificationManager
import android.app.PendingIntent
import android.content.Intent
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
    @Volatile
    private var running = false

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

        startForeground(NOTIFICATION_ID, buildNotification("Подключение..."))
        broadcastState("connecting")

        // Configure VPN interface
        val builder = Builder()
            .setSession("CallVPN")
            .addAddress("10.0.0.2", 32)
            .addRoute("0.0.0.0", 0)
            .addDnsServer("8.8.8.8")
            .addDnsServer("8.8.4.4")
            .setMtu(1280)
            .setBlocking(true)

        vpnInterface = builder.establish()
        if (vpnInterface == null) {
            broadcastState("disconnected")
            stopForeground(STOP_FOREGROUND_REMOVE)
            return
        }

        // Start Go tunnel
        val config = TunnelConfig().apply {
            this.callLink = callLink
            this.serverAddr = serverAddr
            this.numConns = numConns.toLong()
            this.useTCP = true
            this.token = token
        }

        tunnel = Tunnel().also { t ->
            try {
                t.start(config)
            } catch (e: Exception) {
                vpnInterface?.close()
                broadcastState("disconnected")
                stopForeground(STOP_FOREGROUND_REMOVE)
                return
            }
        }

        running = true
        broadcastState("connected")

        val mgr = getSystemService(NotificationManager::class.java)
        mgr.notify(NOTIFICATION_ID, buildNotification("Подключён"))

        // Log poller: reads logs from Go tunnel and broadcasts to UI
        Thread {
            while (running) {
                try {
                    val logs = tunnel?.readLogs() ?: ""
                    if (logs.isNotEmpty()) {
                        tunnel?.clearLogs()
                        val logIntent = Intent(ACTION_LOG).apply {
                            putExtra(EXTRA_LOG_TEXT, logs)
                        }
                        LocalBroadcastManager.getInstance(this).sendBroadcast(logIntent)
                    }
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
            val input = FileInputStream(vpnInterface!!.fileDescriptor)
            val buf = ByteArray(1500)
            while (running) {
                try {
                    val len = input.read(buf)
                    if (len > 0) {
                        tunnel?.writePacket(buf.copyOf(len))
                    }
                } catch (e: Exception) {
                    if (running) continue else break
                }
            }
        }.start()

        // Read from tunnel -> write to TUN
        Thread {
            val output = FileOutputStream(vpnInterface!!.fileDescriptor)
            val buf = ByteArray(1500)
            while (running) {
                try {
                    val len = tunnel?.readPacket(buf) ?: break
                    if (len > 0) {
                        output.write(buf, 0, len.toInt())
                    }
                } catch (e: Exception) {
                    if (running) continue else break
                }
            }
        }.start()
    }

    private fun stopVpn() {
        running = false
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
        const val CHANNEL_ID = "callvpn_channel"
        const val NOTIFICATION_ID = 1
    }
}
