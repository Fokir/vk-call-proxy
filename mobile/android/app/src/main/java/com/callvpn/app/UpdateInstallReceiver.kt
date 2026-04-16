package com.callvpn.app

import android.app.PendingIntent
import android.content.BroadcastReceiver
import android.content.Context
import android.content.Intent
import android.content.pm.PackageInstaller
import android.util.Log
import android.widget.Toast

/**
 * Receives the terminal outcome of a PackageInstaller session started by
 * [UpdateManager]. When the system reports STATUS_PENDING_USER_ACTION, it
 * forwards the user-facing confirm Intent from the extras so the install
 * dialog pops up over the current Activity.
 */
class UpdateInstallReceiver : BroadcastReceiver() {
    companion object {
        private const val TAG = "UpdateInstallReceiver"
    }

    override fun onReceive(context: Context, intent: Intent) {
        val status = intent.getIntExtra(PackageInstaller.EXTRA_STATUS, -1)
        val message = intent.getStringExtra(PackageInstaller.EXTRA_STATUS_MESSAGE) ?: ""

        when (status) {
            PackageInstaller.STATUS_PENDING_USER_ACTION -> {
                val confirm = intent.getParcelableExtra<Intent>(Intent.EXTRA_INTENT)
                if (confirm != null) {
                    confirm.addFlags(Intent.FLAG_ACTIVITY_NEW_TASK)
                    context.startActivity(confirm)
                } else {
                    Log.w(TAG, "PENDING_USER_ACTION without EXTRA_INTENT")
                }
            }
            PackageInstaller.STATUS_SUCCESS -> {
                Log.i(TAG, "update installed")
                Toast.makeText(context, "Update installed", Toast.LENGTH_SHORT).show()
            }
            PackageInstaller.STATUS_FAILURE,
            PackageInstaller.STATUS_FAILURE_ABORTED,
            PackageInstaller.STATUS_FAILURE_BLOCKED,
            PackageInstaller.STATUS_FAILURE_CONFLICT,
            PackageInstaller.STATUS_FAILURE_INCOMPATIBLE,
            PackageInstaller.STATUS_FAILURE_INVALID,
            PackageInstaller.STATUS_FAILURE_STORAGE -> {
                Log.w(TAG, "install failed status=$status: $message")
                Toast.makeText(context, "Update failed: $message", Toast.LENGTH_LONG).show()
            }
            else -> Log.d(TAG, "install status=$status: $message")
        }
    }
}
