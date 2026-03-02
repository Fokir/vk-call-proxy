package com.callvpn.app

import android.content.Intent
import android.net.VpnService
import android.os.ParcelFileDescriptor
import bind.Tunnel
import bind.TunnelConfig
import java.io.FileInputStream
import java.io.FileOutputStream

class CallVpnService : VpnService() {

    private var tunnel: Tunnel? = null
    private var vpnInterface: ParcelFileDescriptor? = null
    @Volatile
    private var running = false

    override fun onStartCommand(intent: Intent?, flags: Int, startId: Int): Int {
        when (intent?.action) {
            ACTION_START -> startVpn(intent)
            ACTION_STOP -> stopVpn()
        }
        return START_STICKY
    }

    private fun startVpn(intent: Intent) {
        val callLink = intent.getStringExtra(EXTRA_CALL_LINK) ?: return
        val serverAddr = intent.getStringExtra(EXTRA_SERVER_ADDR) ?: return
        val numConns = intent.getIntExtra(EXTRA_NUM_CONNS, 4)

        // Configure VPN interface
        val builder = Builder()
            .setSession("CallVPN")
            .addAddress("10.0.0.2", 32)
            .addRoute("0.0.0.0", 0)
            .addDnsServer("8.8.8.8")
            .addDnsServer("8.8.4.4")
            .setMtu(1280)
            .setBlocking(true)

        vpnInterface = builder.establish() ?: return

        // Start Go tunnel
        val config = TunnelConfig().apply {
            this.callLink = callLink
            this.serverAddr = serverAddr
            this.numConns = numConns.toLong()
            this.useTCP = true
        }

        tunnel = Tunnel().also { t ->
            try {
                t.start(config)
            } catch (e: Exception) {
                vpnInterface?.close()
                return
            }
        }

        running = true

        // Read from TUN → write to tunnel
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

        // Read from tunnel → write to TUN
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
        stopSelf()
    }

    override fun onDestroy() {
        stopVpn()
        super.onDestroy()
    }

    companion object {
        const val ACTION_START = "com.callvpn.START"
        const val ACTION_STOP = "com.callvpn.STOP"
        const val EXTRA_CALL_LINK = "call_link"
        const val EXTRA_SERVER_ADDR = "server_addr"
        const val EXTRA_NUM_CONNS = "num_conns"
    }
}
