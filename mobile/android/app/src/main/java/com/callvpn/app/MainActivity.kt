package com.callvpn.app

import android.Manifest
import android.content.BroadcastReceiver
import android.content.Context
import android.content.Intent
import android.content.IntentFilter
import android.content.pm.PackageManager
import android.net.VpnService
import android.os.Build
import android.os.Bundle
import android.widget.Toast
import androidx.activity.ComponentActivity
import androidx.activity.compose.setContent
import androidx.activity.result.contract.ActivityResultContracts
import androidx.compose.foundation.clickable
import androidx.compose.foundation.layout.*
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.shape.CircleShape
import androidx.compose.foundation.verticalScroll
import androidx.compose.material3.*
import androidx.compose.runtime.*
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.platform.ClipboardManager
import androidx.compose.ui.platform.LocalClipboardManager
import androidx.compose.ui.text.AnnotatedString
import androidx.compose.ui.text.font.FontFamily
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.text.input.KeyboardType
import androidx.compose.ui.text.input.PasswordVisualTransformation
import androidx.compose.foundation.text.KeyboardOptions
import androidx.compose.ui.unit.dp
import androidx.compose.ui.unit.sp
import androidx.core.content.ContextCompat
import androidx.localbroadcastmanager.content.LocalBroadcastManager

enum class VpnState { Disconnected, Connecting, Connected }
enum class ConnectionMode { Relay, Direct }

class MainActivity : ComponentActivity() {

    private var vpnState = mutableStateOf(VpnState.Disconnected)
    private var activeConns = mutableStateOf(0)
    private var totalConns = mutableStateOf(0)
    private var logLines = mutableStateOf<List<String>>(emptyList())
    private var pendingCallLink = ""
    private var pendingServerAddr = ""
    private var pendingToken = ""

    private val vpnPermissionLauncher = registerForActivityResult(
        ActivityResultContracts.StartActivityForResult()
    ) { result ->
        if (result.resultCode == RESULT_OK) {
            getSharedPreferences("callvpn", Context.MODE_PRIVATE)
                .edit().putBoolean("vpn_permission_granted", true).apply()
            startVpnService()
        } else {
            Toast.makeText(this, "VPN permission denied", Toast.LENGTH_SHORT).show()
        }
    }

    private val notificationPermissionLauncher = registerForActivityResult(
        ActivityResultContracts.RequestPermission()
    ) { /* proceed regardless of result */ }

    private val stateReceiver = object : BroadcastReceiver() {
        override fun onReceive(context: Context?, intent: Intent?) {
            val state = intent?.getStringExtra(CallVpnService.EXTRA_STATE) ?: return
            vpnState.value = when (state) {
                "connecting" -> VpnState.Connecting
                "connected" -> VpnState.Connected
                else -> VpnState.Disconnected
            }
            if (state == "disconnected") {
                activeConns.value = 0
                totalConns.value = 0
            }
            if (state == "connecting") {
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

    private val connCountReceiver = object : BroadcastReceiver() {
        override fun onReceive(context: Context?, intent: Intent?) {
            activeConns.value = intent?.getIntExtra(CallVpnService.EXTRA_ACTIVE_CONNS, 0) ?: 0
            totalConns.value = intent?.getIntExtra(CallVpnService.EXTRA_TOTAL_CONNS, 0) ?: 0
        }
    }

    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)

        // Request notification permission on Android 13+.
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.TIRAMISU) {
            if (ContextCompat.checkSelfPermission(this, Manifest.permission.POST_NOTIFICATIONS)
                != PackageManager.PERMISSION_GRANTED
            ) {
                notificationPermissionLauncher.launch(Manifest.permission.POST_NOTIFICATIONS)
            }
        }

        // Sync initial state in case VPN was started from tile before activity.
        vpnState.value = when (CallVpnService.currentState) {
            "connecting" -> VpnState.Connecting
            "connected" -> VpnState.Connected
            else -> VpnState.Disconnected
        }

        val lbm = LocalBroadcastManager.getInstance(this)
        lbm.registerReceiver(stateReceiver, IntentFilter(CallVpnService.ACTION_STATE_CHANGED))
        lbm.registerReceiver(logReceiver, IntentFilter(CallVpnService.ACTION_LOG))
        lbm.registerReceiver(connCountReceiver, IntentFilter(CallVpnService.ACTION_CONN_COUNT))

        handleQuickConnect(intent)

        setContent {
            MaterialTheme(colorScheme = darkColorScheme()) {
                Surface(
                    modifier = Modifier.fillMaxSize(),
                    color = MaterialTheme.colorScheme.background
                ) {
                    CallVpnScreen(
                        vpnState = vpnState.value,
                        activeConns = activeConns.value,
                        totalConns = totalConns.value,
                        logLines = logLines.value,
                        onConnect = { callLink, serverAddr, token, numConns -> requestConnect(callLink, serverAddr, token, numConns) },
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
        lbm.unregisterReceiver(connCountReceiver)
        super.onDestroy()
    }

    private var pendingNumConns = 4

    private fun requestConnect(callLink: String, serverAddr: String, token: String, numConns: Int) {
        pendingCallLink = callLink
        pendingServerAddr = serverAddr
        pendingToken = token
        pendingNumConns = numConns

        val intent = VpnService.prepare(this)
        if (intent != null) {
            vpnPermissionLauncher.launch(intent)
        } else {
            getSharedPreferences("callvpn", Context.MODE_PRIVATE)
                .edit().putBoolean("vpn_permission_granted", true).apply()
            startVpnService()
        }
    }

    private fun startVpnService() {
        vpnState.value = VpnState.Connecting

        val intent = Intent(this, CallVpnService::class.java).apply {
            action = CallVpnService.ACTION_START
            putExtra(CallVpnService.EXTRA_CALL_LINK, pendingCallLink)
            putExtra(CallVpnService.EXTRA_SERVER_ADDR, pendingServerAddr)
            putExtra(CallVpnService.EXTRA_NUM_CONNS, pendingNumConns)
            putExtra(CallVpnService.EXTRA_TOKEN, pendingToken)
        }
        ContextCompat.startForegroundService(this, intent)
    }

    private fun stopVpn() {
        val intent = Intent(this, CallVpnService::class.java).apply {
            action = CallVpnService.ACTION_STOP
        }
        startService(intent)
    }

    override fun onNewIntent(intent: Intent) {
        super.onNewIntent(intent)
        handleQuickConnect(intent)
    }

    private fun handleQuickConnect(intent: Intent?) {
        if (intent?.action != ACTION_QUICK_CONNECT) return
        if (vpnState.value != VpnState.Disconnected) return

        val prefs = getSharedPreferences("callvpn", Context.MODE_PRIVATE)
        val rawCallLink = prefs.getString("call_link", "") ?: ""
        if (rawCallLink.isBlank()) return

        val callLink = parseCallLink(rawCallLink)
        val token = prefs.getString("token", "") ?: ""
        val numConns = prefs.getInt("num_conns", 4)
        val connectionMode = prefs.getString("connection_mode", "Relay") ?: "Relay"
        val serverAddr = if (connectionMode == "Direct") {
            prefs.getString("server_addr", "") ?: ""
        } else ""

        requestConnect(callLink, serverAddr, token, numConns)
    }

    companion object {
        const val ACTION_QUICK_CONNECT = "com.callvpn.QUICK_CONNECT"
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
    activeConns: Int,
    totalConns: Int,
    logLines: List<String>,
    onConnect: (callLink: String, serverAddr: String, token: String, numConns: Int) -> Unit,
    onDisconnect: () -> Unit
) {
    val context = androidx.compose.ui.platform.LocalContext.current
    val prefs = remember {
        context.getSharedPreferences("callvpn", Context.MODE_PRIVATE)
    }

    var callLinkInput by remember { mutableStateOf(prefs.getString("call_link", "") ?: "") }
    var serverAddr by remember { mutableStateOf(prefs.getString("server_addr", "") ?: "") }
    var tokenInput by remember { mutableStateOf(prefs.getString("token", "") ?: "") }
    var numConnsInput by remember { mutableStateOf(prefs.getInt("num_conns", 4).toString()) }
    var connectionMode by remember {
        val saved = prefs.getString("connection_mode", "Relay") ?: "Relay"
        mutableStateOf(if (saved == "Direct") ConnectionMode.Direct else ConnectionMode.Relay)
    }

    // Recent call IDs (up to 5, no duplicates)
    var recentIds by remember {
        val saved = prefs.getString("recent_ids", "") ?: ""
        mutableStateOf(saved.split("\n").filter { it.isNotBlank() }.take(5))
    }

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
        VpnState.Connecting -> "Отмена"
        VpnState.Connected -> "Отключиться"
    }

    // Log auto-scroll state
    val logScrollState = rememberScrollState()
    LaunchedEffect(logLines.size) {
        logScrollState.animateScrollTo(logScrollState.maxValue)
    }

    // Clipboard
    val clipboardManager: ClipboardManager = LocalClipboardManager.current

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

        // Connection count
        if (vpnState != VpnState.Disconnected && totalConns > 0) {
            Text(
                text = "Подключения: $activeConns / $totalConns",
                fontSize = 13.sp,
                color = if (activeConns == totalConns)
                    MaterialTheme.colorScheme.onSurfaceVariant
                else
                    Color(0xFFFFC107),
                fontFamily = FontFamily.Monospace
            )
        }

        // Mode selector
        Row(
            modifier = Modifier.fillMaxWidth(),
            horizontalArrangement = Arrangement.spacedBy(8.dp, Alignment.CenterHorizontally)
        ) {
            FilterChip(
                selected = connectionMode == ConnectionMode.Relay,
                onClick = {
                    connectionMode = ConnectionMode.Relay
                    prefs.edit().putString("connection_mode", connectionMode.name).apply()
                },
                label = { Text("Relay-to-Relay") },
                enabled = !isConnected
            )
            FilterChip(
                selected = connectionMode == ConnectionMode.Direct,
                onClick = {
                    connectionMode = ConnectionMode.Direct
                    prefs.edit().putString("connection_mode", connectionMode.name).apply()
                },
                label = { Text("Direct") },
                enabled = !isConnected
            )
        }

        Spacer(modifier = Modifier.height(24.dp))

        // Big round button
        Button(
            onClick = {
                when (vpnState) {
                    VpnState.Disconnected -> {
                        if (canConnect) {
                            val conns = numConnsInput.toIntOrNull()?.coerceIn(1, 16) ?: 4
                            prefs.edit()
                                .putString("call_link", callLinkInput)
                                .putString("server_addr", serverAddr)
                                .putString("token", tokenInput)
                                .putInt("num_conns", conns)
                                .apply()
                            // Save to recent IDs
                            val updated = (listOf(parsedId) + recentIds.filter { it != parsedId }).take(5)
                            recentIds = updated
                            prefs.edit().putString("recent_ids", updated.joinToString("\n")).apply()

                            val effectiveServerAddr = if (connectionMode == ConnectionMode.Relay) "" else serverAddr
                            onConnect(parsedId, effectiveServerAddr, tokenInput, conns)
                        }
                    }
                    VpnState.Connecting -> onDisconnect()
                    VpnState.Connected -> onDisconnect()
                }
            },
            modifier = Modifier.size(170.dp),
            shape = CircleShape,
            colors = ButtonDefaults.buttonColors(containerColor = buttonColor)
        ) {
            Text(
                text = buttonText,
                fontSize = 16.sp,
                fontWeight = FontWeight.Bold,
                color = Color.White
            )
        }

        // App version
        Text(
            text = "v${BuildConfig.VERSION_NAME}",
            fontSize = 12.sp,
            color = MaterialTheme.colorScheme.onSurfaceVariant
        )

        Spacer(modifier = Modifier.height(24.dp))

        // VK link input
        OutlinedTextField(
            value = callLinkInput,
            onValueChange = { callLinkInput = it },
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

        // Recent call IDs
        if (recentIds.isNotEmpty() && !isConnected) {
            Column(
                modifier = Modifier.fillMaxWidth(),
                verticalArrangement = Arrangement.spacedBy(4.dp)
            ) {
                Text(
                    text = "Недавние",
                    fontSize = 12.sp,
                    color = MaterialTheme.colorScheme.onSurfaceVariant
                )
                for (id in recentIds) {
                    Surface(
                        modifier = Modifier
                            .fillMaxWidth()
                            .clickable { callLinkInput = id },
                        color = MaterialTheme.colorScheme.surfaceVariant,
                        shape = MaterialTheme.shapes.small
                    ) {
                        Text(
                            text = id,
                            fontSize = 13.sp,
                            fontFamily = FontFamily.Monospace,
                            color = MaterialTheme.colorScheme.onSurfaceVariant,
                            modifier = Modifier.padding(horizontal = 12.dp, vertical = 8.dp)
                        )
                    }
                }
            }
        }

        // Server address input (only in Direct mode)
        if (connectionMode == ConnectionMode.Direct) {
            OutlinedTextField(
                value = serverAddr,
                onValueChange = { serverAddr = it },
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

        // Connections count input
        OutlinedTextField(
            value = numConnsInput,
            onValueChange = { value ->
                if (value.isEmpty() || value.all { it.isDigit() }) {
                    numConnsInput = value
                }
            },
            label = { Text("Подключения (1-16)") },
            placeholder = { Text("4") },
            singleLine = true,
            enabled = !isConnected,
            keyboardOptions = KeyboardOptions(keyboardType = KeyboardType.Number),
            modifier = Modifier.fillMaxWidth()
        )

        // Log window (always visible)
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
                .height(150.dp)
                .clickable {
                    if (logLines.isNotEmpty()) {
                        clipboardManager.setText(AnnotatedString(logLines.joinToString("\n")))
                        Toast.makeText(context, "Логи скопированы", Toast.LENGTH_SHORT).show()
                    }
                },
            color = MaterialTheme.colorScheme.surfaceVariant,
            shape = MaterialTheme.shapes.small
        ) {
            if (logLines.isEmpty()) {
                Box(
                    modifier = Modifier.fillMaxSize(),
                    contentAlignment = Alignment.Center
                ) {
                    Text(
                        text = "Нет записей",
                        fontSize = 12.sp,
                        color = MaterialTheme.colorScheme.onSurfaceVariant.copy(alpha = 0.5f)
                    )
                }
            } else {
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
