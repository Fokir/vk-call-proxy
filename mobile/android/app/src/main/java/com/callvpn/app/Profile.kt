package com.callvpn.app

import android.content.Context
import android.content.SharedPreferences
import org.json.JSONArray
import org.json.JSONObject
import java.util.UUID

data class Profile(
    val id: String = UUID.randomUUID().toString(),
    val name: String = "",
    val connectionMode: String = "relay", // "relay" or "direct"
    val callLink: String = "",
    val serverAddr: String = "",
    val token: String = "",
    val numConns: Int = 4
) {
    fun isTelemostLink(): Boolean {
        return callLink.contains("telemost.yandex") ||
                (callLink.all { it.isDigit() } && callLink.length > 10)
    }

    fun toJson(): JSONObject = JSONObject().apply {
        put("id", id)
        put("name", name)
        put("connectionMode", connectionMode)
        put("callLink", callLink)
        put("serverAddr", serverAddr)
        put("token", token)
        put("numConns", numConns)
    }

    companion object {
        fun fromJson(obj: JSONObject): Profile = Profile(
            id = obj.optString("id", UUID.randomUUID().toString()),
            name = obj.optString("name", ""),
            connectionMode = obj.optString("connectionMode", "relay"),
            callLink = obj.optString("callLink", ""),
            serverAddr = obj.optString("serverAddr", ""),
            token = obj.optString("token", ""),
            numConns = obj.optInt("numConns", 4)
        )
    }
}

class ProfileManager(context: Context) {
    private val prefs: SharedPreferences =
        context.getSharedPreferences("callvpn", Context.MODE_PRIVATE)

    init {
        migrateFromLegacy()
    }

    fun getProfiles(): List<Profile> {
        val json = prefs.getString("profiles_json", null) ?: return emptyList()
        return try {
            val arr = JSONArray(json)
            (0 until arr.length()).map { Profile.fromJson(arr.getJSONObject(it)) }
        } catch (_: Exception) {
            emptyList()
        }
    }

    fun getActiveProfileId(): String? {
        return prefs.getString("active_profile_id", null)
    }

    fun getActiveProfile(): Profile? {
        val id = getActiveProfileId() ?: return null
        return getProfiles().find { it.id == id }
    }

    fun setActiveProfileId(id: String?) {
        prefs.edit().putString("active_profile_id", id).apply()
    }

    fun saveProfile(profile: Profile) {
        val profiles = getProfiles().toMutableList()
        val idx = profiles.indexOfFirst { it.id == profile.id }
        if (idx >= 0) {
            profiles[idx] = profile
        } else {
            profiles.add(profile)
        }
        saveProfiles(profiles)
    }

    fun deleteProfile(id: String) {
        val profiles = getProfiles().toMutableList()
        val idx = profiles.indexOfFirst { it.id == id }
        if (idx < 0) return
        profiles.removeAt(idx)
        saveProfiles(profiles)

        // If deleted profile was active, select next available
        if (getActiveProfileId() == id) {
            val nextActive = if (profiles.isNotEmpty()) {
                profiles[idx.coerceAtMost(profiles.size - 1)].id
            } else null
            setActiveProfileId(nextActive)
        }
    }

    private fun saveProfiles(profiles: List<Profile>) {
        val arr = JSONArray()
        profiles.forEach { arr.put(it.toJson()) }
        prefs.edit().putString("profiles_json", arr.toString()).apply()
    }

    private fun migrateFromLegacy() {
        // Already migrated if profiles exist
        if (prefs.contains("profiles_json")) return

        val callLink = prefs.getString("call_link", "") ?: ""
        // Only migrate if there's actual data
        if (callLink.isBlank()) return

        val serverAddr = prefs.getString("server_addr", "") ?: ""
        val token = prefs.getString("token", "") ?: ""
        val numConns = prefs.getInt("num_conns", 4)
        val mode = prefs.getString("connection_mode", "Relay") ?: "Relay"

        val profile = Profile(
            name = "Default",
            connectionMode = if (mode == "Direct") "direct" else "relay",
            callLink = callLink,
            serverAddr = serverAddr,
            token = token,
            numConns = numConns
        )

        saveProfiles(listOf(profile))
        setActiveProfileId(profile.id)

        // Clean up legacy keys
        prefs.edit()
            .remove("call_link")
            .remove("server_addr")
            .remove("token")
            .remove("num_conns")
            .remove("connection_mode")
            .remove("recent_ids")
            .apply()
    }
}
