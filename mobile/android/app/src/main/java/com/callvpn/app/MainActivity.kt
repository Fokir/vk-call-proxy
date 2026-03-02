package com.callvpn.app

import android.app.Activity
import android.content.Intent
import android.net.VpnService
import android.os.Bundle
import android.widget.Button
import android.widget.EditText
import android.widget.Toast

class MainActivity : Activity() {

    private val VPN_REQUEST_CODE = 1

    private lateinit var etCallLink: EditText
    private lateinit var etServerAddr: EditText
    private lateinit var btnStart: Button
    private lateinit var btnStop: Button

    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)

        // Simple programmatic layout
        val layout = android.widget.LinearLayout(this).apply {
            orientation = android.widget.LinearLayout.VERTICAL
            setPadding(32, 32, 32, 32)
        }

        etCallLink = EditText(this).apply { hint = "VK Call Link ID" }
        etServerAddr = EditText(this).apply { hint = "Server Address (host:port)" }
        btnStart = Button(this).apply { text = "Start VPN" }
        btnStop = Button(this).apply { text = "Stop VPN"; isEnabled = false }

        layout.addView(etCallLink)
        layout.addView(etServerAddr)
        layout.addView(btnStart)
        layout.addView(btnStop)
        setContentView(layout)

        btnStart.setOnClickListener { requestVpnPermission() }
        btnStop.setOnClickListener { stopVpn() }
    }

    private fun requestVpnPermission() {
        val intent = VpnService.prepare(this)
        if (intent != null) {
            startActivityForResult(intent, VPN_REQUEST_CODE)
        } else {
            startVpn()
        }
    }

    override fun onActivityResult(requestCode: Int, resultCode: Int, data: Intent?) {
        super.onActivityResult(requestCode, resultCode, data)
        if (requestCode == VPN_REQUEST_CODE && resultCode == RESULT_OK) {
            startVpn()
        } else {
            Toast.makeText(this, "VPN permission denied", Toast.LENGTH_SHORT).show()
        }
    }

    private fun startVpn() {
        val callLink = etCallLink.text.toString().trim()
        val serverAddr = etServerAddr.text.toString().trim()

        if (callLink.isEmpty() || serverAddr.isEmpty()) {
            Toast.makeText(this, "Fill all fields", Toast.LENGTH_SHORT).show()
            return
        }

        val intent = Intent(this, CallVpnService::class.java).apply {
            action = CallVpnService.ACTION_START
            putExtra(CallVpnService.EXTRA_CALL_LINK, callLink)
            putExtra(CallVpnService.EXTRA_SERVER_ADDR, serverAddr)
            putExtra(CallVpnService.EXTRA_NUM_CONNS, 4)
        }
        startService(intent)

        btnStart.isEnabled = false
        btnStop.isEnabled = true
    }

    private fun stopVpn() {
        val intent = Intent(this, CallVpnService::class.java).apply {
            action = CallVpnService.ACTION_STOP
        }
        startService(intent)

        btnStart.isEnabled = true
        btnStop.isEnabled = false
    }
}
