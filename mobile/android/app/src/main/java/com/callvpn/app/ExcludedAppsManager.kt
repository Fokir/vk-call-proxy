package com.callvpn.app

import android.content.Context
import android.content.SharedPreferences
import org.json.JSONArray

class ExcludedAppsManager(context: Context) {
    enum class Mode { BLACKLIST, WHITELIST }
    private val prefs: SharedPreferences =
        context.getSharedPreferences("callvpn", Context.MODE_PRIVATE)

    fun getMode(): Mode {
        val v = prefs.getString(KEY_MODE, null)
        return if (v == "whitelist") Mode.WHITELIST else Mode.BLACKLIST
    }

    fun setMode(mode: Mode) {
        prefs.edit().putString(KEY_MODE, if (mode == Mode.WHITELIST) "whitelist" else "blacklist").apply()
    }

    fun getSelectedPackages(): Set<String> {
        val json = prefs.getString(KEY_EXCLUDED, null)
            ?: return DEFAULT_PACKAGES.toSet()
        return try {
            val arr = JSONArray(json)
            val set = mutableSetOf<String>()
            for (i in 0 until arr.length()) {
                set.add(arr.getString(i))
            }
            if (getMode() == Mode.BLACKLIST) set + FORCED_PACKAGES else set - FORCED_PACKAGES
        } catch (_: Exception) {
            DEFAULT_PACKAGES.toSet()
        }
    }

    /** Backward-compatible alias used by CallVpnService. */
    fun getExcludedPackages(): Set<String> = getSelectedPackages()

    fun setSelectedPackages(packages: Set<String>) {
        val arr = JSONArray()
        val adjusted = if (getMode() == Mode.BLACKLIST) packages + FORCED_PACKAGES else packages - FORCED_PACKAGES
        adjusted.forEach { arr.put(it) }
        prefs.edit().putString(KEY_EXCLUDED, arr.toString()).apply()
    }

    fun setExcludedPackages(packages: Set<String>) = setSelectedPackages(packages)

    fun isInitialized(): Boolean = prefs.contains(KEY_EXCLUDED)

    companion object {
        private const val KEY_EXCLUDED = "excluded_packages_json"
        private const val KEY_MODE = "apps_filter_mode"

        /** Apps that MUST always be excluded — user cannot include them in VPN. */
        val FORCED_PACKAGES = setOf(
            "com.callvpn.app",
            "com.vkontakte.android",
            "com.vk.im",
            "com.vk.admin",
            "com.vk.video",
            "ru.rutube.app",
            "ru.rostel.gosuslugi",
            "ru.mail.max",
        )

        /** Default excluded packages (used on first launch). */
        val DEFAULT_PACKAGES = listOf(
            // CallVPN itself (prevent routing loop)
            "com.callvpn.app",

            // VK
            "com.vkontakte.android",
            "com.vk.im",
            "com.vk.admin",
            "ru.vk.store",

            // VK Video
            "com.vk.video",

            // MAX Messenger
            "ru.mail.max",

            // Yandex
            "com.yandex.browser",
            "com.yandex.browser.lite",
            "ru.yandex.searchplugin",
            "ru.yandex.yandexmaps",
            "ru.yandex.taxi",
            "com.yandex.music",
            "com.yandex.disk",
            "ru.yandex.mail",
            "com.yandex.marketplace",
            "ru.yandex.weatherplugin",
            "ru.yandex.translate",
            "com.yandex.lavka",
            "ru.kinopoisk",
            "ru.kinopoisk.plus",
            "com.yandex.mobile.drive",

            // Mobile operators
            "ru.mts.mymts",
            "ru.megafon.mlk",
            "ru.beeline.services",
            "ru.tele2.mytele2",
            "ru.rt.lk",
            "ru.yota.android",

            // Rutube
            "ru.rutube.app",

            // Okko
            "ru.more.play",

            // Ivi
            "ru.ivi.client",

            // Pochta Rossii
            "ru.russianpost.postapp",

            // GIS ZhKH
            "ru.minstroyrf.giszhkh",

            // Gosuslugi
            "ru.rostel.gosuslugi",

            // Banks
            "ru.sberbankmobile",
            "ru.sberbank.sberbankid",
            "ru.sberbank.spasibo",
            "com.idamob.tinkoff.android",
            "ru.tinkoff.investing",
            "ru.tinkoff.insurance",
            "ru.vtb24.mobilebanking.android",
            "ru.alfabank.mobile.android",
            "ru.raiffeisennews",
            "com.openbank",
            "ru.sovcomcard.halva",
            "ru.rosbank.android",
            "ru.psbank.online",
            "ru.bpc.mobilebank.android",
            "ru.rshb.mbank",
            "ru.mw",
            "com.unicredit.mob",
            "ru.letobank.Prometheus",
            "ru.yoomoney.android",
            "ru.homecredit.mycredit",
        )
    }
}
