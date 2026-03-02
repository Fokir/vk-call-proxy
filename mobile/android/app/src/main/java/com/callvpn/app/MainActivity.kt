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
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.shape.CircleShape
import androidx.compose.foundation.verticalScroll
import androidx.compose.material3.*
import androidx.compose.runtime.*
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.text.font.FontFamily
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.text.input.PasswordVisualTransformation
import androidx.compose.ui.unit.dp
import androidx.compose.ui.unit.sp
import androidx.localbroadcastmanager.content.LocalBroadcastManager

enum class VpnState { Disconnected, Connecting, Connected }
enum class ConnectionMode { Relay, Direct }

class MainActivity : ComponentActivity() {

    private var vpnState = mutableStateOf(VpnState.Disconnected)
    private var logLines = mutableStateOf<List<String>>(emptyList())
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
            if (state == "disconnected") {
                logLines.value = emptyList()
            }
        }
    }

    private val logReceiver = object : BroadcastReceiver() {
        override fun onReceive(context: Context?, intent: Intent?) {
            val text = intent?.getStringExtra(CallVpnService.EXTRA_LOG_TEXT) ?: return
            val newLines = text.split("\n").filter { it.isNotBlank() }
            logLines.value = (logLines.value + newLines).takeLast(20)
        }
    }

    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)

        val lbm = LocalBroadcastManager.getInstance(this)
        lbm.registerReceiver(stateReceiver, IntentFilter(CallVpnService.ACTION_STATE_CHANGED))
        lbm.registerReceiver(logReceiver, IntentFilter(CallVpnService.ACTION_LOG))

        setContent {
            MaterialTheme(colorScheme = darkColorScheme()) {
                Surface(
                    modifier = Modifier.fillMaxSize(),
                    color = MaterialTheme.colorScheme.background
                ) {
                    CallVpnScreen(
                        vpnState = vpnState.value,
                        logLines = logLines.value,
                        onConnect = { callLink, serverAddr, token -> requestConnect(callLink, serverAddr, token) },
                        onDisconnect = { stopVpn() }
                    )
                }
            }
        }
    }

    override fun onDestroy() {
        val lbm = LocalBroadcastManager.getInstance(this)
        lbm.unregisterReceiver(stateReceiver)
        lbm.unregisterReceiver(logReceiver)
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
    logLines: List<String>,
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
    var connectionMode by remember {
        val saved = prefs.getString("connection_mode", "Relay") ?: "Relay"
        mutableStateOf(if (saved == "Direct") ConnectionMode.Direct else ConnectionMode.Relay)
    }
    var showChangeDialog by remember { mutableStateOf(false) }
    var pendingFieldChange by remember { mutableStateOf<(() -> Unit)?>(null) }

    val isConnected = vpnState != VpnState.Disconnected
    val parsedId = remember(callLinkInput) { parseCallLink(callLinkInput) }
    val hasFullLink = remember(callLinkInput) {
        callLinkInput.contains("vk.com/call/join/")
    }

    // Validation
    val canConnect = when (connectionMode) {
        ConnectionMode.Relay -> parsedId.isNotBlank()
        ConnectionMode.Direct -> parsedId.isNotBlank() && serverAddr.isNotBlank()
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

    // Log auto-scroll state
    val logScrollState = rememberScrollState()
    LaunchedEffect(logLines.size) {
        logScrollState.animateScrollTo(logScrollState.maxValue)
    }

    Column(
        modifier = Modifier
            .fillMaxSize()
            .verticalScroll(rememberScrollState())
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

        // Mode selector (segmented button)
        val modeOptions = listOf("Relay-to-Relay", "Direct")
        val selectedIndex = if (connectionMode == ConnectionMode.Relay) 0 else 1
        SingleChoiceSegmentedButtonRow(modifier = Modifier.fillMaxWidth()) {
            modeOptions.forEachIndexed { index, label ->
                SegmentedButton(
                    selected = index == selectedIndex,
                    onClick = {
                        connectionMode = if (index == 0) ConnectionMode.Relay else ConnectionMode.Direct
                        prefs.edit().putString("connection_mode", connectionMode.name).apply()
                    },
                    shape = SegmentedButtonDefaults.itemShape(index = index, count = modeOptions.size),
                    enabled = !isConnected
                ) {
                    Text(label)
                }
            }
        }

        Spacer(modifier = Modifier.height(24.dp))

        // Big round button
        Button(
            onClick = {
                when (vpnState) {
                    VpnState.Disconnected -> {
                        if (canConnect) {
                            prefs.edit()
                                .putString("call_link", callLinkInput)
                                .putString("server_addr", serverAddr)
                                .putString("token", tokenInput)
                                .apply()
                            val effectiveServerAddr = if (connectionMode == ConnectionMode.Relay) "" else serverAddr
                            onConnect(parsedId, effectiveServerAddr, tokenInput)
                        }
                    }
                    VpnState.Connected -> onDisconnect()
                    VpnState.Connecting -> { /* disabled */ }
                }
            },
            modifier = Modifier.size(170.dp),
            shape = CircleShape,
            colors = ButtonDefaults.buttonColors(containerColor = buttonColor),
            enabled = vpnState != VpnState.Connecting
        ) {
            Text(
                text = buttonText,
                fontSize = 16.sp,
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

        // Server address input (only in Direct mode)
        if (connectionMode == ConnectionMode.Direct) {
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
        }

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

        // Log window
        if (logLines.isNotEmpty()) {
            Text(
                text = "Лог",
                fontSize = 14.sp,
                fontWeight = FontWeight.Medium,
                color = MaterialTheme.colorScheme.onSurfaceVariant,
                modifier = Modifier.fillMaxWidth()
            )
            Surface(
                modifier = Modifier
                    .fillMaxWidth()
                    .height(150.dp),
                color = MaterialTheme.colorScheme.surfaceVariant,
                shape = MaterialTheme.shapes.small
            ) {
                Column(
                    modifier = Modifier
                        .verticalScroll(logScrollState)
                        .padding(8.dp)
                ) {
                    for (line in logLines) {
                        Text(
                            text = line,
                            fontSize = 11.sp,
                            fontFamily = FontFamily.Monospace,
                            color = MaterialTheme.colorScheme.onSurfaceVariant
                        )
                    }
                }
            }
        }
    }
}
