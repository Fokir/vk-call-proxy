package com.callvpn.app

import android.app.PendingIntent
import android.content.Context
import android.content.Intent
import android.content.pm.PackageInstaller
import android.os.Build
import android.util.Log
import bind.Tunnel
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.withContext
import java.io.File

/**
 * Handles checking for and installing APK self-updates advertised in the
 * hot-scripts manifest. Relies on Tunnel.APKUpdateVersion/URL/SHA256 and
 * Tunnel.DownloadAPK from the Go bridge.
 *
 * The actual install triggers the system confirm dialog (no silent installs
 * without root/device-owner).
 */
class UpdateManager(private val context: Context) {

    companion object {
        private const val TAG = "UpdateManager"
        private const val INSTALL_ACTION = "com.callvpn.app.UPDATE_INSTALL_RESULT"
    }

    data class Update(
        val version: String,
        val url: String,
        val sha256: String,
    )

    /**
     * Returns the advertised update, or null if manifest has no APK entry or
     * the advertised version matches (or is older than) the installed one.
     */
    fun check(tunnel: Tunnel): Update? {
        val version = tunnel.apkUpdateVersion()
        val url = tunnel.apkUpdateURL()
        val sha = tunnel.apkUpdateSHA256()
        if (version.isNullOrEmpty() || url.isNullOrEmpty()) return null

        val currentName = try {
            context.packageManager.getPackageInfo(context.packageName, 0).versionName ?: ""
        } catch (e: Exception) {
            ""
        }
        if (!isNewer(version, currentName)) return null
        return Update(version = version, url = url, sha256 = sha ?: "")
    }

    /**
     * Downloads the APK through the Go bridge (reuses the manifest's
     * URL + verifies sha256). Suspends until file is on disk.
     */
    suspend fun download(tunnel: Tunnel): File = withContext(Dispatchers.IO) {
        val dir = File(context.cacheDir, "updates").apply { mkdirs() }
        val apk = File(dir, "callvpn-update.apk")
        if (apk.exists()) apk.delete()
        tunnel.downloadAPK(apk.absolutePath)
        apk
    }

    /**
     * Starts the system install dialog for the given APK file. Returns true
     * if the session was committed — the final outcome is delivered to
     * [UpdateInstallReceiver] via the PendingIntent.
     */
    fun install(apk: File): Boolean {
        if (!apk.exists() || apk.length() == 0L) {
            Log.w(TAG, "install: APK missing or empty: ${apk.absolutePath}")
            return false
        }
        val installer = context.packageManager.packageInstaller
        val params = PackageInstaller.SessionParams(PackageInstaller.SessionParams.MODE_FULL_INSTALL)
        val sessionId = installer.createSession(params)
        val session = installer.openSession(sessionId)
        try {
            session.openWrite("base.apk", 0, apk.length()).use { out ->
                apk.inputStream().use { it.copyTo(out) }
                session.fsync(out)
            }
            val intent = Intent(INSTALL_ACTION).setPackage(context.packageName)
            val flags = if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.S) {
                PendingIntent.FLAG_UPDATE_CURRENT or PendingIntent.FLAG_MUTABLE
            } else {
                PendingIntent.FLAG_UPDATE_CURRENT
            }
            val pi = PendingIntent.getBroadcast(context, sessionId, intent, flags)
            session.commit(pi.intentSender)
            Log.i(TAG, "install session committed: id=$sessionId")
            return true
        } catch (e: Exception) {
            Log.e(TAG, "install failed", e)
            session.abandon()
            return false
        } finally {
            session.close()
        }
    }

    /** Compares semver-ish strings ("0.25.0" > "0.24.1"). */
    private fun isNewer(advertised: String, installed: String): Boolean {
        if (installed.isEmpty()) return true
        val a = advertised.split('.').mapNotNull { it.toIntOrNull() }
        val b = installed.split('.').mapNotNull { it.toIntOrNull() }
        val n = maxOf(a.size, b.size)
        for (i in 0 until n) {
            val x = a.getOrElse(i) { 0 }
            val y = b.getOrElse(i) { 0 }
            if (x != y) return x > y
        }
        return false
    }
}
