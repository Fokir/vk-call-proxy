package com.callvpn.app

import android.content.BroadcastReceiver
import android.content.Context
import android.content.Intent
import android.content.IntentFilter
import android.net.VpnService
import android.os.Bundle
import android.widget.Toast
import androidx.activity.ComponentActivity
import androidx.activity.compose.setContent
import androidx.activity.result.contract.ActivityResultContracts
import androidx.compose.foundation.layout.*
import androidx.compose.foundation.shape.CircleShape
import androidx.compose.material3.*
import androidx.compose.runtime.*
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.text.input.PasswordVisualTransformation
import androidx.compose.ui.unit.dp
import androidx.compose.ui.unit.sp
import androidx.localbroadcastmanager.content.LocalBroadcastManager

enum class VpnState { Disconnected, Connecting, Connected }

class MainActivity : ComponentActivity() {

    private var vpnState = mutableStateOf(VpnState.Disconnected)
    private var pendingCallLink = ""
    private var pendingServerAddr = ""
    private var pendingToken = ""

    private val vpnPermissionLauncher = registerForActivityResult(
        ActivityResultContracts.StartActivityForResult()
    ) { result ->
        if (result.resultCode == RESULT_OK) {
            startVpnService()
        } else {
            Toast.makeText(this, "VPN permission denied", Toast.LENGTH_SHORT).show()
        }
    }

    private val stateReceiver = object : BroadcastReceiver() {
        override fun onReceive(context: Context?, intent: Intent?) {
            val state = intent?.getStringExtra(CallVpnService.EXTRA_STATE) ?: return
            vpnState.value = when (state) {
                "connecting" -> VpnState.Connecting
                "connected" -> VpnState.Connected
                else -> VpnState.Disconnected
            }
        }
    }

    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)

        LocalBroadcastManager.getInstance(this).registerReceiver(
            stateReceiver,
            IntentFilter(CallVpnService.ACTION_STATE_CHANGED)
        )

        setContent {
            MaterialTheme(colorScheme = darkColorScheme()) {
                Surface(
                    modifier = Modifier.fillMaxSize(),
                    color = MaterialTheme.colorScheme.background
                ) {
                    CallVpnScreen(
                        vpnState = vpnState.value,
                        onConnect = { callLink, serverAddr, token -> requestConnect(callLink, serverAddr, token) },
                        onDisconnect = { stopVpn() }
                    )
                }
            }
        }
    }

    override fun onDestroy() {
        LocalBroadcastManager.getInstance(this).unregisterReceiver(stateReceiver)
        super.onDestroy()
    }

    private fun requestConnect(callLink: String, serverAddr: String, token: String) {
        pendingCallLink = callLink
        pendingServerAddr = serverAddr
        pendingToken = token

        val intent = VpnService.prepare(this)
        if (intent != null) {
            vpnPermissionLauncher.launch(intent)
        } else {
            startVpnService()
        }
    }

    private fun startVpnService() {
        vpnState.value = VpnState.Connecting

        val intent = Intent(this, CallVpnService::class.java).apply {
            action = CallVpnService.ACTION_START
            putExtra(CallVpnService.EXTRA_CALL_LINK, pendingCallLink)
            putExtra(CallVpnService.EXTRA_SERVER_ADDR, pendingServerAddr)
            putExtra(CallVpnService.EXTRA_NUM_CONNS, 4)
            putExtra(CallVpnService.EXTRA_TOKEN, pendingToken)
        }
        startService(intent)
    }

    private fun stopVpn() {
        val intent = Intent(this, CallVpnService::class.java).apply {
            action = CallVpnService.ACTION_STOP
        }
        startService(intent)
    }
}

private fun parseCallLink(input: String): String {
    val regex = Regex("""vk\.com/call/join/([A-Za-z0-9_-]+)""")
    val match = regex.find(input)
    return match?.groupValues?.get(1) ?: input.trim()
}

@OptIn(ExperimentalMaterial3Api::class)
@Composable
fun CallVpnScreen(
    vpnState: VpnState,
    onConnect: (callLink: String, serverAddr: String, token: String) -> Unit,
    onDisconnect: () -> Unit
) {
    val context = androidx.compose.ui.platform.LocalContext.current
    val prefs = remember {
        context.getSharedPreferences("callvpn", Context.MODE_PRIVATE)
    }

    var callLinkInput by remember { mutableStateOf(prefs.getString("call_link", "") ?: "") }
    var serverAddr by remember { mutableStateOf(prefs.getString("server_addr", "") ?: "") }
    var tokenInput by remember { mutableStateOf(prefs.getString("token", "") ?: "") }
    var showChangeDialog by remember { mutableStateOf(false) }
    var pendingFieldChange by remember { mutableStateOf<(() -> Unit)?>(null) }

    val isConnected = vpnState != VpnState.Disconnected
    val parsedId = remember(callLinkInput) { parseCallLink(callLinkInput) }
    val hasFullLink = remember(callLinkInput) {
        callLinkInput.contains("vk.com/call/join/")
    }

    // Status colors
    val statusColor = when (vpnState) {
        VpnState.Disconnected -> Color.Gray
        VpnState.Connecting -> Color(0xFFFFC107)
        VpnState.Connected -> Color(0xFF4CAF50)
    }
    val statusText = when (vpnState) {
        VpnState.Disconnected -> "Отключён"
        VpnState.Connecting -> "Подключение..."
        VpnState.Connected -> "Подключён"
    }

    // Button config
    val buttonColor = when (vpnState) {
        VpnState.Disconnected -> Color(0xFF4CAF50)
        VpnState.Connecting -> Color(0xFFFFC107)
        VpnState.Connected -> Color(0xFFF44336)
    }
    val buttonText = when (vpnState) {
        VpnState.Disconnected -> "Подключиться"
        VpnState.Connecting -> "Подключение..."
        VpnState.Connected -> "Отключиться"
    }

    // Confirmation dialog
    if (showChangeDialog) {
        AlertDialog(
            onDismissRequest = {
                showChangeDialog = false
                pendingFieldChange = null
            },
            title = { Text("Изменить значение?") },
            text = { Text("Сохранённое значение будет заменено.") },
            confirmButton = {
                TextButton(onClick = {
                    pendingFieldChange?.invoke()
                    showChangeDialog = false
                    pendingFieldChange = null
                }) {
                    Text("Изменить")
                }
            },
            dismissButton = {
                TextButton(onClick = {
                    showChangeDialog = false
                    pendingFieldChange = null
                }) {
                    Text("Отмена")
                }
            }
        )
    }

    Column(
        modifier = Modifier
            .fillMaxSize()
            .padding(24.dp),
        horizontalAlignment = Alignment.CenterHorizontally,
        verticalArrangement = Arrangement.spacedBy(16.dp)
    ) {
        Spacer(modifier = Modifier.height(32.dp))

        // Title
        Text(
            text = "CallVPN",
            fontSize = 32.sp,
            fontWeight = FontWeight.Bold,
            color = MaterialTheme.colorScheme.onBackground
        )

        // Status
        Text(
            text = statusText,
            fontSize = 16.sp,
            color = statusColor,
            fontWeight = FontWeight.Medium
        )

        Spacer(modifier = Modifier.height(24.dp))

        // Big round button
        Button(
            onClick = {
                when (vpnState) {
                    VpnState.Disconnected -> {
                        if (parsedId.isNotBlank() && serverAddr.isNotBlank()) {
                            prefs.edit()
                                .putString("call_link", callLinkInput)
                                .putString("server_addr", serverAddr)
                                .putString("token", tokenInput)
                                .apply()
                            onConnect(parsedId, serverAddr, tokenInput)
                        }
                    }
                    VpnState.Connected -> onDisconnect()
                    VpnState.Connecting -> { /* disabled */ }
                }
            },
            modifier = Modifier.size(140.dp),
            shape = CircleShape,
            colors = ButtonDefaults.buttonColors(containerColor = buttonColor),
            enabled = vpnState != VpnState.Connecting
        ) {
            Text(
                text = buttonText,
                fontSize = 14.sp,
                fontWeight = FontWeight.Bold,
                color = Color.White
            )
        }

        Spacer(modifier = Modifier.height(24.dp))

        // VK link input
        OutlinedTextField(
            value = callLinkInput,
            onValueChange = { newValue ->
                val savedValue = prefs.getString("call_link", "") ?: ""
                if (savedValue.isNotEmpty() && savedValue != callLinkInput && callLinkInput == savedValue) {
                    pendingFieldChange = { callLinkInput = newValue }
                    showChangeDialog = true
                } else {
                    callLinkInput = newValue
                }
            },
            label = { Text("Ссылка VK звонка") },
            placeholder = { Text("https://vk.com/call/join/...") },
            singleLine = true,
            enabled = !isConnected,
            modifier = Modifier.fillMaxWidth()
        )

        // Show parsed ID if full link was pasted
        if (hasFullLink && parsedId.isNotBlank()) {
            Text(
                text = "ID: $parsedId",
                fontSize = 12.sp,
                color = MaterialTheme.colorScheme.onSurfaceVariant
            )
        }

        // Server address input
        OutlinedTextField(
            value = serverAddr,
            onValueChange = { newValue ->
                val savedValue = prefs.getString("server_addr", "") ?: ""
                if (savedValue.isNotEmpty() && savedValue != serverAddr && serverAddr == savedValue) {
                    pendingFieldChange = { serverAddr = newValue }
                    showChangeDialog = true
                } else {
                    serverAddr = newValue
                }
            },
            label = { Text("Адрес сервера") },
            placeholder = { Text("host:port") },
            singleLine = true,
            enabled = !isConnected,
            modifier = Modifier.fillMaxWidth()
        )

        // Token input
        OutlinedTextField(
            value = tokenInput,
            onValueChange = { tokenInput = it },
            label = { Text("Токен") },
            placeholder = { Text("Токен авторизации") },
            singleLine = true,
            enabled = !isConnected,
            visualTransformation = PasswordVisualTransformation(),
            modifier = Modifier.fillMaxWidth()
        )
    }
}
