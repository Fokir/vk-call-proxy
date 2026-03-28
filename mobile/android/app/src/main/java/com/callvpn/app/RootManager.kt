package com.callvpn.app

import android.content.Context
import android.content.SharedPreferences
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.withContext
import java.io.BufferedReader
import java.io.File
import java.io.InputStreamReader

/**
 * Manages root detection, WiFi hotspot routing through VPN TUN interface,
 * and TTL normalization to hide tethering from mobile operators.
 */
class RootManager(context: Context) {

    private val prefs: SharedPreferences =
        context.getSharedPreferences("callvpn", Context.MODE_PRIVATE)

    // ── Persisted toggle ──────────────────────────────────────────────

    var hotspotRoutingEnabled: Boolean
        get() = prefs.getBoolean(KEY_HOTSPOT_ROUTING, false)
        set(value) { prefs.edit().putBoolean(KEY_HOTSPOT_ROUTING, value).apply() }

    // ── Root detection ────────────────────────────────────────────────

    @Volatile
    private var rootAvailable: Boolean? = null

    /**
     * Async root check. Tries `su -c id` and caches the result.
     * Returns true if device has root AND user granted su access.
     * Safe to call from coroutine (runs on Dispatchers.IO).
     */
    suspend fun isRootAvailable(): Boolean {
        rootAvailable?.let { return it }
        return withContext(Dispatchers.IO) {
            val result = checkRoot()
            rootAvailable = result
            result
        }
    }

    /** Synchronous cached getter. Returns false if check hasn't run yet. */
    fun isRootAvailableCached(): Boolean = rootAvailable ?: false

    private fun checkRoot(): Boolean {
        // Step 1: Check if su binary exists anywhere
        val suPaths = listOf(
            "/system/bin/su", "/system/xbin/su",
            "/sbin/su", "/su/bin/su",
            "/data/local/xbin/su", "/data/local/bin/su"
        )
        val suExists = suPaths.any { File(it).exists() }
        if (!suExists) return false

        // Step 2: Execute su -c id to verify actual access
        return try {
            val process = Runtime.getRuntime().exec(arrayOf("su", "-c", "id"))
            val reader = BufferedReader(InputStreamReader(process.inputStream))
            val output = reader.readLine() ?: ""
            val exitCode = process.waitFor()
            exitCode == 0 && output.contains("uid=0")
        } catch (_: Exception) {
            false
        }
    }

    // ── iptables management ───────────────────────────────────────────

    // Tethering interface wildcards (AOSP, Samsung, Huawei, MediaTek, etc.)
    private val TETHER_INTERFACES = listOf("wlan+", "ap+", "swlan+", "rndis+")

    /**
     * Sets up iptables rules to route WiFi hotspot traffic through the VPN TUN.
     * Also fixes TTL to 64 to hide tethering from the mobile operator.
     *
     * Must be called AFTER the TUN interface is established.
     * Safe to call on a background thread (blocks while su executes).
     *
     * @param tunName The TUN interface name (e.g., "tun0")
     * @return true if all commands succeeded
     */
    fun setupHotspotRouting(tunName: String): Boolean {
        // Lazy sync root check (safe on background thread)
        if (rootAvailable == null) {
            rootAvailable = checkRoot()
        }
        if (!isRootAvailableCached()) return false

        // Clean up stale rules from a possible previous crash
        executeRootCommands(buildCleanupCommands(tunName))

        // Run all setup commands (no set -e: some interface patterns may not
        // exist on this device, which is fine — we only need the active one).
        executeRootCommands(buildSetupCommands(tunName))

        // Verify the critical route is in place. Android netd can flush custom
        // routing tables, and some su implementations don't fully process piped
        // commands. Retry the route in a separate su session if missing.
        for (attempt in 1..3) {
            val hasRoute = executeRootCommand("ip route show table 200")
                .contains("dev $tunName")
            if (hasRoute) return true

            // Route missing — re-add it
            Thread.sleep(200L * attempt)
            executeRootCommands(listOf(
                "ip route replace default dev $tunName table 200"
            ))
        }

        lastError = "route in table 200 did not persist after 3 attempts"
        return false
    }

    /**
     * Removes all hotspot routing rules.
     * Must be called BEFORE the TUN interface is torn down.
     */
    fun cleanupHotspotRouting(tunName: String) {
        if (rootAvailable != true) return
        executeRootCommands(buildCleanupCommands(tunName))
    }

    private fun buildSetupCommands(tunName: String): List<String> {
        val cmds = mutableListOf<String>()

        // 1. Enable IP forwarding (sysctl works reliably under su, echo > /proc does not)
        cmds += "sysctl -w net.ipv4.ip_forward=1"

        // 2. Mark packets from tethering interfaces (fwmark 2)
        for (iface in TETHER_INTERFACES) {
            cmds += "iptables -t mangle -I PREROUTING -i $iface -j MARK --set-mark 2"
        }

        // 3. Policy routing: marked packets → VPN TUN
        cmds += "ip rule add fwmark 2 table 200 pref 100"
        cmds += "ip route replace default dev $tunName table 200"

        // 4. NAT tethered traffic through TUN
        cmds += "iptables -t nat -I POSTROUTING -o $tunName -j MASQUERADE"

        // 5. FORWARD rules for tethering → TUN and return traffic
        for (iface in TETHER_INTERFACES) {
            cmds += "iptables -I FORWARD -i $iface -o $tunName -j ACCEPT"
            cmds += "iptables -I FORWARD -o $iface -m state --state RELATED,ESTABLISHED -j ACCEPT"
        }

        // 6. Fix TTL to 64 on all outgoing packets (hides tethering from operator)
        cmds += "iptables -t mangle -I POSTROUTING -j TTL --ttl-set 64"

        // 7. Block IPv6 forwarding for tethered devices — forces fallback to IPv4
        //    which is routed through VPN. Without this, sites that prefer IPv6 (e.g.
        //    whatsmyipaddress.com, linkmeter.net) leak the real operator IP.
        for (iface in TETHER_INTERFACES) {
            cmds += "ip6tables -I FORWARD -i $iface -j DROP"
            cmds += "ip6tables -I FORWARD -o $iface -j DROP"
        }

        return cmds
    }

    private fun buildCleanupCommands(tunName: String): List<String> {
        val cmds = mutableListOf<String>()

        // Reverse order of setup. Use -D (delete). 2>/dev/null for tolerance.
        for (iface in TETHER_INTERFACES) {
            cmds += "ip6tables -D FORWARD -o $iface -j DROP 2>/dev/null"
            cmds += "ip6tables -D FORWARD -i $iface -j DROP 2>/dev/null"
        }

        cmds += "iptables -t mangle -D POSTROUTING -j TTL --ttl-set 64 2>/dev/null"

        for (iface in TETHER_INTERFACES) {
            cmds += "iptables -D FORWARD -o $iface -m state --state RELATED,ESTABLISHED -j ACCEPT 2>/dev/null"
            cmds += "iptables -D FORWARD -i $iface -o $tunName -j ACCEPT 2>/dev/null"
        }

        cmds += "iptables -t nat -D POSTROUTING -o $tunName -j MASQUERADE 2>/dev/null"

        cmds += "ip route flush table 200 2>/dev/null"
        cmds += "ip rule del fwmark 2 table 200 2>/dev/null"

        for (iface in TETHER_INTERFACES) {
            cmds += "iptables -t mangle -D PREROUTING -i $iface -j MARK --set-mark 2 2>/dev/null"
        }

        cmds += "sysctl -w net.ipv4.ip_forward=0"

        return cmds
    }

    /** Last stderr output from executeRootCommands (for diagnostics). */
    var lastError: String = ""
        private set

    /**
     * Executes commands via su by writing a temp script file and running it.
     * More reliable than piping to stdin: avoids buffering/timing issues
     * across Magisk, KernelSU, SuperSU implementations.
     */
    private fun executeRootCommands(commands: List<String>): Boolean {
        return try {
            val scriptFile = File.createTempFile("cvpn_", ".sh")
            scriptFile.writeText(commands.joinToString("\n") + "\n")
            scriptFile.setReadable(true, false)
            val path = scriptFile.absolutePath
            val process = Runtime.getRuntime().exec(arrayOf("su", "-c", "sh $path"))
            val stderr = process.errorStream.bufferedReader().readText()
            val exitCode = process.waitFor()
            scriptFile.delete()
            lastError = stderr.trim()
            exitCode == 0
        } catch (e: Exception) {
            lastError = e.message ?: e.toString()
            false
        }
    }

    /**
     * Executes a single command via su and returns its stdout.
     * Used for verification (e.g. `ip route show table 200`).
     */
    private fun executeRootCommand(command: String): String {
        return try {
            val process = Runtime.getRuntime().exec(arrayOf("su", "-c", command))
            val stdout = process.inputStream.bufferedReader().readText()
            process.waitFor()
            stdout.trim()
        } catch (_: Exception) {
            ""
        }
    }

    companion object {
        private const val KEY_HOTSPOT_ROUTING = "hotspot_routing_enabled"
    }
}
