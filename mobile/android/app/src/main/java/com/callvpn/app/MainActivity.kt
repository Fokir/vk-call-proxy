package com.callvpn.app

import android.Manifest
import android.content.BroadcastReceiver
import android.content.Context
import android.content.Intent
import android.content.IntentFilter
import android.content.pm.PackageManager
import android.net.VpnService
import android.net.Uri
import android.os.Build
import android.os.Bundle
import android.os.PowerManager
import android.provider.Settings
import android.widget.Toast
import androidx.activity.ComponentActivity
import androidx.activity.compose.setContent
import androidx.activity.result.contract.ActivityResultContracts
import androidx.compose.foundation.BorderStroke
import androidx.compose.foundation.clickable
import androidx.compose.foundation.layout.*
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.items
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.shape.CircleShape
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.foundation.verticalScroll
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.filled.Add
import androidx.compose.material.icons.filled.Close
import androidx.compose.material.icons.filled.Edit
import androidx.compose.material.icons.filled.Search
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
import androidx.compose.ui.window.Dialog
import androidx.compose.ui.window.DialogProperties
import androidx.core.content.ContextCompat
import androidx.localbroadcastmanager.content.LocalBroadcastManager
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.withContext

enum class VpnState { Disconnected, Connecting, Connected }

class MainActivity : ComponentActivity() {

    private var vpnState = mutableStateOf(VpnState.Disconnected)
    private var activeConns = mutableStateOf(0)
    private var totalConns = mutableStateOf(0)
    private var logLines = mutableStateOf<List<String>>(emptyList())
    private var connectionStage = mutableStateOf("")
    private var speedTestRunning = mutableStateOf(false)
    private var speedTestPhase = mutableStateOf("")
    private var speedTestProgress = mutableStateOf("")
    private var speedTestResult = mutableStateOf("")
    private var pendingCallLink = ""
    private var pendingServerAddr = ""
    private var pendingToken = ""
    private var pendingVkTokens = ""

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
        }
    }

    private val logReceiver = object : BroadcastReceiver() {
        override fun onReceive(context: Context?, intent: Intent?) {
            val text = intent?.getStringExtra(CallVpnService.EXTRA_LOG_TEXT) ?: return
            val newLines = text.split("\n").filter { it.isNotBlank() }
            logLines.value = (logLines.value + newLines).takeLast(500)
        }
    }

    private val connCountReceiver = object : BroadcastReceiver() {
        override fun onReceive(context: Context?, intent: Intent?) {
            activeConns.value = intent?.getIntExtra(CallVpnService.EXTRA_ACTIVE_CONNS, 0) ?: 0
            totalConns.value = intent?.getIntExtra(CallVpnService.EXTRA_TOTAL_CONNS, 0) ?: 0
        }
    }

    private val stageReceiver = object : BroadcastReceiver() {
        override fun onReceive(context: Context?, intent: Intent?) {
            connectionStage.value = intent?.getStringExtra(CallVpnService.EXTRA_STAGE_TEXT) ?: ""
        }
    }

    private val speedTestReceiver = object : BroadcastReceiver() {
        override fun onReceive(context: Context, intent: Intent) {
            val phase = intent.getStringExtra(CallVpnService.EXTRA_SPEEDTEST_PHASE) ?: ""
            val json = intent.getStringExtra(CallVpnService.EXTRA_SPEEDTEST_JSON) ?: ""

            when (phase) {
                "complete" -> {
                    speedTestRunning.value = false
                    speedTestResult.value = json
                    speedTestProgress.value = ""
                }
                "error" -> {
                    speedTestRunning.value = false
                    speedTestResult.value = json
                    speedTestProgress.value = ""
                }
                else -> {
                    speedTestPhase.value = phase
                    if (json.isNotEmpty()) {
                        speedTestProgress.value = json
                    }
                }
            }
        }
    }

    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)

        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.TIRAMISU) {
            if (ContextCompat.checkSelfPermission(this, Manifest.permission.POST_NOTIFICATIONS)
                != PackageManager.PERMISSION_GRANTED
            ) {
                notificationPermissionLauncher.launch(Manifest.permission.POST_NOTIFICATIONS)
            }
        }

        requestBatteryOptimizationExemption()

        vpnState.value = when (CallVpnService.currentState) {
            "connecting" -> VpnState.Connecting
            "connected" -> VpnState.Connected
            else -> VpnState.Disconnected
        }

        val lbm = LocalBroadcastManager.getInstance(this)
        lbm.registerReceiver(stateReceiver, IntentFilter(CallVpnService.ACTION_STATE_CHANGED))
        lbm.registerReceiver(logReceiver, IntentFilter(CallVpnService.ACTION_LOG))
        lbm.registerReceiver(connCountReceiver, IntentFilter(CallVpnService.ACTION_CONN_COUNT))
        lbm.registerReceiver(stageReceiver, IntentFilter(CallVpnService.ACTION_STAGE))
        lbm.registerReceiver(speedTestReceiver, IntentFilter(CallVpnService.ACTION_SPEEDTEST_PROGRESS))

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
                        connectionStage = connectionStage.value,
                        logLines = logLines.value,
                        speedTestRunning = speedTestRunning.value,
                        speedTestPhase = speedTestPhase.value,
                        speedTestProgress = speedTestProgress.value,
                        speedTestResult = speedTestResult.value,
                        onSpeedTestStart = {
                            speedTestRunning.value = true
                            speedTestResult.value = ""
                            speedTestProgress.value = ""
                            speedTestPhase.value = ""
                            val intent = Intent(this, CallVpnService::class.java).apply {
                                action = CallVpnService.ACTION_SPEEDTEST_START
                            }
                            startService(intent)
                        },
                        onConnect = { callLink, serverAddr, token, numConns, vkTokens -> requestConnect(callLink, serverAddr, token, numConns, vkTokens) },
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
        lbm.unregisterReceiver(stageReceiver)
        lbm.unregisterReceiver(speedTestReceiver)
        super.onDestroy()
    }

    private var pendingNumConns = 4

    private fun requestConnect(callLink: String, serverAddr: String, token: String, numConns: Int, vkTokens: String = "") {
        pendingCallLink = callLink
        pendingServerAddr = serverAddr
        pendingToken = token
        pendingNumConns = numConns
        pendingVkTokens = vkTokens

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
            putExtra(CallVpnService.EXTRA_VK_TOKENS, pendingVkTokens)
        }
        ContextCompat.startForegroundService(this, intent)
    }

    private fun requestBatteryOptimizationExemption() {
        val pm = getSystemService(PowerManager::class.java) ?: return
        if (!pm.isIgnoringBatteryOptimizations(packageName)) {
            val intent = Intent(Settings.ACTION_REQUEST_IGNORE_BATTERY_OPTIMIZATIONS).apply {
                data = Uri.parse("package:$packageName")
            }
            try { startActivity(intent) } catch (_: Exception) { }
        }
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

        val profileManager = ProfileManager(this)
        val profile = profileManager.getActiveProfile() ?: return
        val callLink = profile.callLinks
            .map { parseCallLink(it) }
            .filter { it.isNotBlank() }
            .joinToString(",")
        if (callLink.isBlank()) return

        val serverAddr = if (profile.connectionMode == "direct") profile.serverAddr else ""
        requestConnect(callLink, serverAddr, profile.token, profile.numConns, profile.vkTokensJoined())
    }

    companion object {
        const val ACTION_QUICK_CONNECT = "com.callvpn.QUICK_CONNECT"
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

@OptIn(ExperimentalMaterial3Api::class, ExperimentalLayoutApi::class)
@Composable
fun CallVpnScreen(
    vpnState: VpnState,
    activeConns: Int,
    totalConns: Int,
    connectionStage: String = "",
    logLines: List<String>,
    speedTestRunning: Boolean = false,
    speedTestPhase: String = "",
    speedTestProgress: String = "",
    speedTestResult: String = "",
    onSpeedTestStart: () -> Unit = {},
    onConnect: (callLink: String, serverAddr: String, token: String, numConns: Int, vkTokens: String) -> Unit,
    onDisconnect: () -> Unit
) {
    val context = androidx.compose.ui.platform.LocalContext.current
    val profileManager = remember { ProfileManager(context) }

    var profiles by remember { mutableStateOf(profileManager.getProfiles()) }
    var activeProfileId by remember { mutableStateOf(profileManager.getActiveProfileId()) }
    var showEditor by remember { mutableStateOf(false) }
    var editingProfile by remember { mutableStateOf<Profile?>(null) }

    // Excluded apps
    var showExcludedApps by remember { mutableStateOf(false) }

    // Root detection for WiFi hotspot routing
    val rootManager = remember { RootManager(context) }
    var rootAvailable by remember { mutableStateOf(false) }
    var hotspotRouting by remember { mutableStateOf(rootManager.hotspotRoutingEnabled) }

    LaunchedEffect(Unit) {
        rootAvailable = rootManager.isRootAvailable()
        // If root was lost since last session, disable the toggle
        if (!rootAvailable && hotspotRouting) {
            hotspotRouting = false
            rootManager.hotspotRoutingEnabled = false
        }
    }

    val activeProfile = profiles.find { it.id == activeProfileId }
    val isConnected = vpnState != VpnState.Disconnected

    // Status colors
    val statusColor = when (vpnState) {
        VpnState.Disconnected -> Color.Gray
        VpnState.Connecting -> Color(0xFFFFC107)
        VpnState.Connected -> Color(0xFF4CAF50)
    }
    val statusText = when (vpnState) {
        VpnState.Disconnected -> "Отключён"
        VpnState.Connecting -> if (connectionStage.isNotEmpty()) connectionStage else "Подключение..."
        VpnState.Connected -> "Подключён"
    }
    val buttonColor = when (vpnState) {
        VpnState.Disconnected -> if (activeProfile != null) Color(0xFF4CAF50) else Color.Gray
        VpnState.Connecting -> Color(0xFFFFC107)
        VpnState.Connected -> Color(0xFFF44336)
    }
    val buttonText = when (vpnState) {
        VpnState.Disconnected -> "Подключиться"
        VpnState.Connecting -> "Отмена"
        VpnState.Connected -> "Отключиться"
    }

    val logScrollState = rememberScrollState()
    LaunchedEffect(logLines.size) {
        logScrollState.animateScrollTo(logScrollState.maxValue)
    }

    val clipboardManager: ClipboardManager = LocalClipboardManager.current

    fun connectProfile(profile: Profile) {
        val callLink = profile.callLinks
            .map { parseCallLink(it) }
            .filter { it.isNotBlank() }
            .joinToString(",")
        val serverAddr = if (profile.connectionMode == "direct") profile.serverAddr else ""
        val conns = profile.numConns.coerceIn(1, 16)
        val vkTokensStr = profile.vkTokensJoined()
        onConnect(callLink, serverAddr, profile.token, conns, vkTokensStr)
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

        Spacer(modifier = Modifier.height(8.dp))

        // Profile badges
        FlowRow(
            modifier = Modifier.fillMaxWidth(),
            horizontalArrangement = Arrangement.spacedBy(8.dp),
            verticalArrangement = Arrangement.spacedBy(8.dp)
        ) {
            for (profile in profiles) {
                val isActive = profile.id == activeProfileId
                ProfileBadge(
                    profile = profile,
                    isActive = isActive,
                    onSelect = {
                        if (!isActive) {
                            // Disconnect previous if connected
                            if (isConnected) {
                                onDisconnect()
                            }
                            activeProfileId = profile.id
                            profileManager.setActiveProfileId(profile.id)
                            // Auto-connect
                            connectProfile(profile)
                        }
                    },
                    onEdit = {
                        editingProfile = profile
                        showEditor = true
                    }
                )
            }

            // Add profile button
            Surface(
                modifier = Modifier
                    .height(40.dp)
                    .clickable {
                        editingProfile = null
                        showEditor = true
                    },
                shape = RoundedCornerShape(20.dp),
                border = BorderStroke(1.dp, MaterialTheme.colorScheme.outline),
                color = MaterialTheme.colorScheme.surface
            ) {
                Row(
                    modifier = Modifier.padding(horizontal = 16.dp),
                    verticalAlignment = Alignment.CenterVertically,
                    horizontalArrangement = Arrangement.spacedBy(6.dp)
                ) {
                    Icon(
                        Icons.Default.Add,
                        contentDescription = "Добавить",
                        modifier = Modifier.size(18.dp),
                        tint = MaterialTheme.colorScheme.onSurfaceVariant
                    )
                    Text(
                        "Добавить",
                        fontSize = 13.sp,
                        color = MaterialTheme.colorScheme.onSurfaceVariant
                    )
                }
            }
        }

        Spacer(modifier = Modifier.height(24.dp))

        // Big round button
        Button(
            onClick = {
                when (vpnState) {
                    VpnState.Disconnected -> {
                        val profile = activeProfile ?: return@Button
                        connectProfile(profile)
                    }
                    VpnState.Connecting -> onDisconnect()
                    VpnState.Connected -> onDisconnect()
                }
            },
            modifier = Modifier.size(170.dp),
            shape = CircleShape,
            enabled = vpnState != VpnState.Disconnected || activeProfile != null,
            colors = ButtonDefaults.buttonColors(
                containerColor = buttonColor,
                disabledContainerColor = Color.Gray
            )
        ) {
            Text(
                text = buttonText,
                fontSize = 16.sp,
                fontWeight = FontWeight.Bold,
                color = Color.White
            )
        }

        // Excluded apps button
        OutlinedButton(
            onClick = { showExcludedApps = true },
            modifier = Modifier.fillMaxWidth(),
            shape = RoundedCornerShape(12.dp),
            border = BorderStroke(1.dp, MaterialTheme.colorScheme.outline)
        ) {
            Text(
                text = "Исключённые приложения",
                fontSize = 14.sp,
                color = MaterialTheme.colorScheme.onSurface
            )
        }

        // WiFi Hotspot routing toggle (only visible if root is available)
        if (rootAvailable) {
            Spacer(modifier = Modifier.height(16.dp))
            Row(
                modifier = Modifier
                    .fillMaxWidth()
                    .padding(horizontal = 16.dp),
                verticalAlignment = Alignment.CenterVertically,
                horizontalArrangement = Arrangement.SpaceBetween
            ) {
                Column(modifier = Modifier.weight(1f)) {
                    Text(
                        text = "WiFi Hotspot через VPN",
                        fontSize = 14.sp,
                        fontWeight = FontWeight.Medium,
                        color = MaterialTheme.colorScheme.onSurface
                    )
                    Text(
                        text = "Маршрутизация раздачи + TTL 64",
                        fontSize = 11.sp,
                        color = MaterialTheme.colorScheme.onSurfaceVariant
                    )
                }
                Switch(
                    checked = hotspotRouting,
                    onCheckedChange = { enabled ->
                        hotspotRouting = enabled
                        rootManager.hotspotRoutingEnabled = enabled
                    },
                    enabled = vpnState == VpnState.Disconnected,
                    colors = SwitchDefaults.colors(
                        checkedThumbColor = Color.White,
                        checkedTrackColor = Color(0xFF4CAF50),
                        uncheckedThumbColor = Color.White,
                        uncheckedTrackColor = MaterialTheme.colorScheme.surfaceVariant
                    )
                )
            }
        }

        // App version
        Text(
            text = "v${BuildConfig.VERSION_NAME}",
            fontSize = 12.sp,
            color = MaterialTheme.colorScheme.onSurfaceVariant
        )

        Spacer(modifier = Modifier.height(24.dp))

        // Log window
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
                .height(300.dp)
                .clickable {
                    if (logLines.isNotEmpty()) {
                        clipboardManager.setText(AnnotatedString(logLines.joinToString("\n")))
                        Toast
                            .makeText(context, "Логи скопированы", Toast.LENGTH_SHORT)
                            .show()
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
                        val lineColor = when {
                            line.contains("level=ERROR") || line.startsWith("ERROR:") -> Color(0xFFEF5350)
                            line.contains("level=WARN") -> Color(0xFFFFC107)
                            else -> MaterialTheme.colorScheme.onSurfaceVariant
                        }
                        Text(
                            text = line,
                            fontSize = 11.sp,
                            fontFamily = FontFamily.Monospace,
                            color = lineColor
                        )
                    }
                }
            }
        }

        // Speed Test — visible only when connected
        if (vpnState == VpnState.Connected) {
            Spacer(modifier = Modifier.height(12.dp))

            Button(
                onClick = { onSpeedTestStart() },
                enabled = !speedTestRunning,
                modifier = Modifier.fillMaxWidth()
            ) {
                Text(if (speedTestRunning) "Тестирование..." else "Тест скорости")
            }

            // Progress display
            if (speedTestRunning && speedTestProgress.isNotEmpty()) {
                Spacer(modifier = Modifier.height(8.dp))
                val phaseLabel = speedTestPhase.replaceFirstChar { it.uppercase() }
                val progressText = runCatching {
                    val json = org.json.JSONObject(speedTestProgress)
                    val mbps = json.optDouble("current_mbps", 0.0)
                    val elapsed = json.optInt("elapsed_s", 0)
                    "$phaseLabel: ${"%.2f".format(mbps)} Mbps (${elapsed}s)"
                }.getOrDefault("$phaseLabel...")
                Text(
                    text = progressText,
                    style = MaterialTheme.typography.bodyMedium,
                    color = MaterialTheme.colorScheme.primary
                )
            }

            // Final results
            if (speedTestResult.isNotEmpty() && !speedTestRunning) {
                Spacer(modifier = Modifier.height(8.dp))
                Card(
                    modifier = Modifier.fillMaxWidth(),
                    colors = CardDefaults.cardColors(
                        containerColor = MaterialTheme.colorScheme.surfaceVariant
                    )
                ) {
                    Column(modifier = Modifier.padding(12.dp)) {
                        val lines = runCatching {
                            val json = org.json.JSONObject(speedTestResult)
                            val ping = json.optJSONObject("ping")
                            val download = json.optJSONObject("download")
                            val upload = json.optJSONObject("upload")
                            val conns = json.optJSONArray("connections")
                            buildList {
                                if (ping != null) {
                                    add("Ping: ${"%.1f".format(ping.optDouble("avg_ms"))}ms " +
                                        "(jitter ${"%.1f".format(ping.optDouble("jitter_ms"))}ms)")
                                }
                                if (download != null) {
                                    add("Download: ${"%.2f".format(download.optDouble("mbps"))} Mbps")
                                }
                                if (upload != null) {
                                    add("Upload: ${"%.2f".format(upload.optDouble("mbps"))} Mbps")
                                }
                                if (conns != null && conns.length() > 0) {
                                    add("")  // spacer marker
                                    add("Connections:")
                                    for (i in 0 until conns.length()) {
                                        val c = conns.getJSONObject(i)
                                        add("  #${c.optInt("index")}: " +
                                            "${"%.2f".format(c.optDouble("down_mbps"))}/" +
                                            "${"%.2f".format(c.optDouble("up_mbps"))} Mbps, " +
                                            "${"%.0f".format(c.optDouble("latency_ms"))}ms")
                                    }
                                }
                            }
                        }.getOrNull()

                        if (lines != null) {
                            for (line in lines) {
                                if (line.isEmpty()) {
                                    Spacer(modifier = Modifier.height(4.dp))
                                } else if (line == "Connections:") {
                                    Text(line, style = MaterialTheme.typography.labelSmall)
                                } else if (line.startsWith("  #")) {
                                    Text(line, style = MaterialTheme.typography.bodySmall,
                                        fontFamily = FontFamily.Monospace)
                                } else {
                                    Text(line, style = MaterialTheme.typography.bodyMedium)
                                }
                            }
                        } else {
                            Text(speedTestResult, style = MaterialTheme.typography.bodySmall)
                        }
                    }
                }
            }
        }
    }

    // Excluded apps dialog
    if (showExcludedApps) {
        ExcludedAppsDialog(onDismiss = { showExcludedApps = false })
    }

    // Profile editor dialog
    if (showEditor) {
        ProfileEditorDialog(
            profile = editingProfile,
            onSave = { saved ->
                profileManager.saveProfile(saved)
                profiles = profileManager.getProfiles()

                // If no active profile yet, make this one active
                if (activeProfileId == null) {
                    activeProfileId = saved.id
                    profileManager.setActiveProfileId(saved.id)
                }

                // If edited profile is active and connected, reconnect
                if (saved.id == activeProfileId && isConnected) {
                    onDisconnect()
                    connectProfile(saved)
                }

                showEditor = false
                editingProfile = null
            },
            onDelete = if (editingProfile != null) {
                { id ->
                    val wasActive = id == activeProfileId
                    if (wasActive && isConnected) {
                        onDisconnect()
                    }
                    profileManager.deleteProfile(id)
                    profiles = profileManager.getProfiles()
                    activeProfileId = profileManager.getActiveProfileId()

                    showEditor = false
                    editingProfile = null
                }
            } else null,
            onDismiss = {
                showEditor = false
                editingProfile = null
            }
        )
    }
}

@Composable
fun ProfileBadge(
    profile: Profile,
    isActive: Boolean,
    onSelect: () -> Unit,
    onEdit: () -> Unit
) {
    val bgColor = if (isActive) Color(0xFF4CAF50) else MaterialTheme.colorScheme.surface
    val textColor = if (isActive) Color.White else MaterialTheme.colorScheme.onSurface
    val borderColor = if (isActive) Color(0xFF4CAF50) else MaterialTheme.colorScheme.outline

    Surface(
        modifier = Modifier
            .height(40.dp)
            .clickable { onSelect() },
        shape = RoundedCornerShape(20.dp),
        color = bgColor,
        border = BorderStroke(1.dp, borderColor)
    ) {
        Row(
            modifier = Modifier.padding(start = 12.dp, end = 4.dp),
            verticalAlignment = Alignment.CenterVertically,
            horizontalArrangement = Arrangement.spacedBy(6.dp)
        ) {
            // Provider icon
            Text(
                text = if (profile.isTelemostLink()) "Я" else "VK",
                fontSize = 12.sp,
                fontWeight = FontWeight.Bold,
                color = if (isActive) Color.White else {
                    if (profile.isTelemostLink()) Color(0xFFFF0000) else Color(0xFF0077FF)
                }
            )

            // Profile name
            Text(
                text = profile.name.ifBlank { "Без имени" },
                fontSize = 13.sp,
                fontWeight = FontWeight.Medium,
                color = textColor
            )

            // Edit button
            IconButton(
                onClick = onEdit,
                modifier = Modifier.size(32.dp)
            ) {
                Icon(
                    Icons.Default.Edit,
                    contentDescription = "Редактировать",
                    modifier = Modifier.size(16.dp),
                    tint = textColor.copy(alpha = 0.7f)
                )
            }
        }
    }
}

@OptIn(ExperimentalMaterial3Api::class)
@Composable
fun ProfileEditorDialog(
    profile: Profile?,
    onSave: (Profile) -> Unit,
    onDelete: ((String) -> Unit)?,
    onDismiss: () -> Unit
) {
    val isNew = profile == null
    val base = profile ?: Profile()

    var name by remember { mutableStateOf(base.name) }
    var connectionMode by remember { mutableStateOf(base.connectionMode) }
    var callLinks by remember { mutableStateOf(base.callLinks.ifEmpty { listOf("") }) }
    var serverAddr by remember { mutableStateOf(base.serverAddr) }
    var token by remember { mutableStateOf(base.token) }
    var numConns by remember { mutableStateOf(base.numConns.toString()) }
    var vkTokens by remember { mutableStateOf(base.vkTokens) }
    var showDeleteConfirm by remember { mutableStateOf(false) }

    Dialog(onDismissRequest = onDismiss) {
        Surface(
            shape = RoundedCornerShape(16.dp),
            color = MaterialTheme.colorScheme.surface,
            tonalElevation = 6.dp
        ) {
            Column(
                modifier = Modifier
                    .padding(24.dp)
                    .verticalScroll(rememberScrollState()),
                verticalArrangement = Arrangement.spacedBy(16.dp)
            ) {
                Text(
                    text = if (isNew) "Новый профиль" else "Редактирование",
                    fontSize = 20.sp,
                    fontWeight = FontWeight.Bold,
                    color = MaterialTheme.colorScheme.onSurface
                )

                // Profile name
                OutlinedTextField(
                    value = name,
                    onValueChange = { if (it.length <= 20) name = it },
                    label = { Text("Имя профиля") },
                    placeholder = { Text("Мой VPN") },
                    singleLine = true,
                    modifier = Modifier.fillMaxWidth()
                )

                // Connection mode
                Text(
                    "Тип подключения",
                    fontSize = 12.sp,
                    color = MaterialTheme.colorScheme.onSurfaceVariant
                )
                Row(
                    modifier = Modifier.fillMaxWidth(),
                    horizontalArrangement = Arrangement.spacedBy(8.dp)
                ) {
                    FilterChip(
                        selected = connectionMode == "relay",
                        onClick = { connectionMode = "relay" },
                        label = { Text("Relay-to-Relay") }
                    )
                    FilterChip(
                        selected = connectionMode == "direct",
                        onClick = { connectionMode = "direct" },
                        label = { Text("Direct") }
                    )
                }

                // Call links (1-8)
                Row(
                    modifier = Modifier.fillMaxWidth(),
                    horizontalArrangement = Arrangement.SpaceBetween,
                    verticalAlignment = Alignment.CenterVertically
                ) {
                    Text(
                        "Ссылки на звонок (${callLinks.size}/8)",
                        fontSize = 12.sp,
                        color = MaterialTheme.colorScheme.onSurfaceVariant
                    )
                    if (callLinks.size < 8) {
                        IconButton(
                            onClick = { callLinks = callLinks + "" },
                            modifier = Modifier.size(32.dp)
                        ) {
                            Icon(Icons.Default.Add, "Добавить ссылку", modifier = Modifier.size(18.dp))
                        }
                    }
                }
                callLinks.forEachIndexed { index, link ->
                    Row(
                        modifier = Modifier.fillMaxWidth(),
                        verticalAlignment = Alignment.CenterVertically,
                        horizontalArrangement = Arrangement.spacedBy(4.dp)
                    ) {
                        OutlinedTextField(
                            value = link,
                            onValueChange = { value ->
                                callLinks = callLinks.toMutableList().also { it[index] = value }
                            },
                            label = { Text("Ссылка ${index + 1}") },
                            placeholder = { Text("https://vk.com/call/join/...") },
                            singleLine = true,
                            modifier = Modifier.weight(1f)
                        )
                        if (callLinks.size > 1) {
                            IconButton(
                                onClick = {
                                    callLinks = callLinks.toMutableList().also { it.removeAt(index) }
                                },
                                modifier = Modifier.size(32.dp)
                            ) {
                                Icon(
                                    Icons.Default.Close,
                                    "Удалить",
                                    modifier = Modifier.size(18.dp),
                                    tint = Color(0xFFF44336)
                                )
                            }
                        }
                    }
                }

                // Server address (direct only)
                if (connectionMode == "direct") {
                    OutlinedTextField(
                        value = serverAddr,
                        onValueChange = { serverAddr = it },
                        label = { Text("Адрес сервера") },
                        placeholder = { Text("host:port") },
                        singleLine = true,
                        modifier = Modifier.fillMaxWidth()
                    )
                }

                // Token
                OutlinedTextField(
                    value = token,
                    onValueChange = { token = it },
                    label = { Text("Токен") },
                    placeholder = { Text("Токен авторизации") },
                    singleLine = true,
                    visualTransformation = PasswordVisualTransformation(),
                    modifier = Modifier.fillMaxWidth()
                )

                // Connections count
                OutlinedTextField(
                    value = numConns,
                    onValueChange = { value ->
                        if (value.isEmpty() || value.all { it.isDigit() }) {
                            numConns = value
                        }
                    },
                    label = { Text("Подключения (1-16)") },
                    placeholder = { Text("4") },
                    singleLine = true,
                    keyboardOptions = KeyboardOptions(keyboardType = KeyboardType.Number),
                    modifier = Modifier.fillMaxWidth()
                )

                // VK tokens (0-16)
                Row(
                    modifier = Modifier.fillMaxWidth(),
                    horizontalArrangement = Arrangement.SpaceBetween,
                    verticalAlignment = Alignment.CenterVertically
                ) {
                    Text(
                        "VK токены (${vkTokens.size}/16)",
                        fontSize = 12.sp,
                        color = MaterialTheme.colorScheme.onSurfaceVariant
                    )
                    if (vkTokens.size < 16) {
                        IconButton(
                            onClick = { vkTokens = vkTokens + "" },
                            modifier = Modifier.size(32.dp)
                        ) {
                            Icon(Icons.Default.Add, "Добавить токен", modifier = Modifier.size(18.dp))
                        }
                    }
                }
                if (vkTokens.isEmpty()) {
                    Text(
                        "Ускоряют подключение, обходят rate-limit VK API",
                        fontSize = 11.sp,
                        color = MaterialTheme.colorScheme.onSurfaceVariant.copy(alpha = 0.6f)
                    )
                }
                vkTokens.forEachIndexed { index, vkToken ->
                    Row(
                        modifier = Modifier.fillMaxWidth(),
                        verticalAlignment = Alignment.CenterVertically,
                        horizontalArrangement = Arrangement.spacedBy(4.dp)
                    ) {
                        OutlinedTextField(
                            value = vkToken,
                            onValueChange = { value ->
                                vkTokens = vkTokens.toMutableList().also { it[index] = value }
                            },
                            label = { Text("Токен ${index + 1}") },
                            placeholder = { Text("vk1.a.…") },
                            singleLine = true,
                            visualTransformation = PasswordVisualTransformation(),
                            modifier = Modifier.weight(1f)
                        )
                        IconButton(
                            onClick = {
                                vkTokens = vkTokens.toMutableList().also { it.removeAt(index) }
                            },
                            modifier = Modifier.size(32.dp)
                        ) {
                            Icon(
                                Icons.Default.Close,
                                "Удалить",
                                modifier = Modifier.size(18.dp),
                                tint = Color(0xFFF44336)
                            )
                        }
                    }
                }

                // Delete button (only for existing profiles)
                if (!isNew && onDelete != null) {
                    TextButton(
                        onClick = { showDeleteConfirm = true },
                        modifier = Modifier.fillMaxWidth()
                    ) {
                        Text(
                            "Удалить профиль",
                            color = Color(0xFFF44336)
                        )
                    }
                }

                // Action buttons
                Row(
                    modifier = Modifier.fillMaxWidth(),
                    horizontalArrangement = Arrangement.spacedBy(12.dp, Alignment.End)
                ) {
                    TextButton(onClick = onDismiss) {
                        Text("Отмена")
                    }
                    Button(
                        onClick = {
                            val conns = numConns.toIntOrNull()?.coerceIn(1, 16) ?: 4
                            val saved = base.copy(
                                name = name.trim(),
                                connectionMode = connectionMode,
                                callLinks = callLinks.map { it.trim() },
                                serverAddr = serverAddr.trim(),
                                token = token,
                                numConns = conns,
                                vkTokens = vkTokens.map { it.trim() }.filter { it.isNotEmpty() }
                            )
                            onSave(saved)
                        },
                        colors = ButtonDefaults.buttonColors(
                            containerColor = Color(0xFF4CAF50)
                        )
                    ) {
                        Text("Сохранить", color = Color.White)
                    }
                }
            }
        }
    }

    // Delete confirmation dialog
    if (showDeleteConfirm && onDelete != null && profile != null) {
        AlertDialog(
            onDismissRequest = { showDeleteConfirm = false },
            title = { Text("Удалить профиль?") },
            text = { Text("Профиль \"${profile.name.ifBlank { "Без имени" }}\" будет удалён.") },
            confirmButton = {
                Button(
                    onClick = {
                        showDeleteConfirm = false
                        onDelete(profile.id)
                    },
                    colors = ButtonDefaults.buttonColors(containerColor = Color(0xFFF44336))
                ) {
                    Text("Удалить", color = Color.White)
                }
            },
            dismissButton = {
                TextButton(onClick = { showDeleteConfirm = false }) {
                    Text("Отмена")
                }
            }
        )
    }
}

data class AppInfo(
    val packageName: String,
    val label: String,
)

@OptIn(ExperimentalMaterial3Api::class)
@Composable
fun ExcludedAppsDialog(onDismiss: () -> Unit) {
    val context = androidx.compose.ui.platform.LocalContext.current
    val excludedAppsManager = remember { ExcludedAppsManager(context) }

    var allApps by remember { mutableStateOf<List<AppInfo>>(emptyList()) }
    var loading by remember { mutableStateOf(true) }
    var selectedPackages by remember { mutableStateOf(excludedAppsManager.getSelectedPackages()) }
    var searchQuery by remember { mutableStateOf("") }
    var isWhitelist by remember { mutableStateOf(excludedAppsManager.getMode() == ExcludedAppsManager.Mode.WHITELIST) }

    // Load installed apps in background
    LaunchedEffect(Unit) {
        val apps = withContext(Dispatchers.IO) {
            val pm = context.packageManager
            try {
                pm.getInstalledApplications(0)
                    .mapNotNull { appInfo ->
                        try {
                            val intent = pm.getLaunchIntentForPackage(appInfo.packageName)
                            if (intent == null) return@mapNotNull null
                            AppInfo(
                                packageName = appInfo.packageName,
                                label = try { appInfo.loadLabel(pm).toString() } catch (_: Exception) { appInfo.packageName },
                            )
                        } catch (_: Exception) { null }
                    }
                    .sortedBy { it.label.lowercase() }
            } catch (_: Exception) {
                emptyList()
            }
        }
        allApps = apps
        loading = false
    }

    val filteredApps = remember(allApps, searchQuery) {
        if (searchQuery.isBlank()) allApps
        else {
            val q = searchQuery.lowercase()
            allApps.filter {
                it.label.lowercase().contains(q) || it.packageName.lowercase().contains(q)
            }
        }
    }

    Dialog(
        onDismissRequest = onDismiss,
        properties = DialogProperties(usePlatformDefaultWidth = false)
    ) {
        Surface(
            modifier = Modifier
                .fillMaxWidth(0.95f)
                .fillMaxHeight(0.85f),
            shape = RoundedCornerShape(16.dp),
            color = MaterialTheme.colorScheme.surface,
            tonalElevation = 6.dp
        ) {
            Column(modifier = Modifier.fillMaxSize()) {
                // Header
                Text(
                    text = if (isWhitelist) "Разрешённые приложения" else "Исключённые приложения",
                    fontSize = 20.sp,
                    fontWeight = FontWeight.Bold,
                    color = MaterialTheme.colorScheme.onSurface,
                    modifier = Modifier.padding(start = 24.dp, top = 24.dp, end = 24.dp, bottom = 8.dp)
                )
                Text(
                    text = if (isWhitelist) "Только отмеченные приложения используют VPN"
                           else "Отмеченные приложения не используют VPN",
                    fontSize = 12.sp,
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                    modifier = Modifier.padding(horizontal = 24.dp)
                )

                // Mode toggle
                Row(
                    modifier = Modifier
                        .fillMaxWidth()
                        .padding(horizontal = 24.dp, vertical = 8.dp),
                    verticalAlignment = Alignment.CenterVertically
                ) {
                    Text(
                        text = if (isWhitelist) "Белый список" else "Чёрный список",
                        fontSize = 14.sp,
                        fontWeight = FontWeight.Medium,
                        color = MaterialTheme.colorScheme.onSurface,
                        modifier = Modifier.weight(1f)
                    )
                    Switch(
                        checked = isWhitelist,
                        onCheckedChange = {
                            isWhitelist = it
                            selectedPackages = emptySet()
                        },
                        colors = SwitchDefaults.colors(
                            checkedThumbColor = Color.White,
                            checkedTrackColor = Color(0xFF4CAF50)
                        )
                    )
                }

                Spacer(modifier = Modifier.height(4.dp))

                // Search field
                OutlinedTextField(
                    value = searchQuery,
                    onValueChange = { searchQuery = it },
                    placeholder = { Text("Поиск приложений") },
                    leadingIcon = {
                        Icon(Icons.Default.Search, contentDescription = null, modifier = Modifier.size(20.dp))
                    },
                    singleLine = true,
                    modifier = Modifier
                        .fillMaxWidth()
                        .padding(horizontal = 16.dp),
                    shape = RoundedCornerShape(12.dp)
                )

                Spacer(modifier = Modifier.height(8.dp))

                // App list
                if (loading) {
                    Box(
                        modifier = Modifier
                            .weight(1f)
                            .fillMaxWidth(),
                        contentAlignment = Alignment.Center
                    ) {
                        Text(
                            "Загрузка приложений...",
                            fontSize = 14.sp,
                            color = MaterialTheme.colorScheme.onSurfaceVariant
                        )
                    }
                } else {
                    LazyColumn(
                        modifier = Modifier.weight(1f)
                    ) {
                        items(filteredApps, key = { it.packageName }) { app ->
                            val isForced = app.packageName in ExcludedAppsManager.FORCED_PACKAGES
                            // In blacklist: forced apps are always checked (excluded) and disabled.
                            // In whitelist: forced apps are always unchecked (not allowed) and disabled.
                            val isLocked = isForced
                            val isSelected = if (isForced) {
                                !isWhitelist // blacklist → checked, whitelist → unchecked
                            } else {
                                app.packageName in selectedPackages
                            }

                            Row(
                                modifier = Modifier
                                    .fillMaxWidth()
                                    .clickable(enabled = !isLocked) {
                                        selectedPackages = if (isSelected) {
                                            selectedPackages - app.packageName
                                        } else {
                                            selectedPackages + app.packageName
                                        }
                                    }
                                    .padding(horizontal = 16.dp, vertical = 4.dp),
                                verticalAlignment = Alignment.CenterVertically
                            ) {
                                Column(modifier = Modifier.weight(1f)) {
                                    Text(
                                        text = app.label,
                                        fontSize = 14.sp,
                                        fontWeight = FontWeight.Medium,
                                        color = MaterialTheme.colorScheme.onSurface
                                    )
                                    Text(
                                        text = app.packageName,
                                        fontSize = 11.sp,
                                        color = MaterialTheme.colorScheme.onSurfaceVariant
                                    )
                                }
                                Checkbox(
                                    checked = isSelected,
                                    onCheckedChange = if (isLocked) null else { checked ->
                                        selectedPackages = if (checked) {
                                            selectedPackages + app.packageName
                                        } else {
                                            selectedPackages - app.packageName
                                        }
                                    },
                                    enabled = !isLocked,
                                    colors = CheckboxDefaults.colors(
                                        checkedColor = Color(0xFF4CAF50),
                                        checkmarkColor = Color.White
                                    )
                                )
                            }
                        }
                    }
                }

                // Bottom buttons
                Row(
                    modifier = Modifier
                        .fillMaxWidth()
                        .padding(16.dp),
                    horizontalArrangement = Arrangement.spacedBy(12.dp, Alignment.End)
                ) {
                    TextButton(onClick = onDismiss) {
                        Text("Отмена")
                    }
                    Button(
                        onClick = {
                            excludedAppsManager.setMode(
                                if (isWhitelist) ExcludedAppsManager.Mode.WHITELIST
                                else ExcludedAppsManager.Mode.BLACKLIST
                            )
                            excludedAppsManager.setSelectedPackages(selectedPackages)
                            onDismiss()
                        },
                        colors = ButtonDefaults.buttonColors(
                            containerColor = Color(0xFF4CAF50)
                        )
                    ) {
                        Text("Сохранить", color = Color.White)
                    }
                }
            }
        }
    }
}
