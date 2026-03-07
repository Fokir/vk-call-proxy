import SwiftUI
import UIKit
import NetworkExtension

@main
struct CallVPNApp: App {
    var body: some Scene {
        WindowGroup {
            ContentView()
                .preferredColorScheme(.dark)
        }
    }
}

enum VpnState {
    case disconnected
    case connecting
    case connected

    var statusText: String {
        switch self {
        case .disconnected: return "Отключён"
        case .connecting: return "Подключение..."
        case .connected: return "Подключён"
        }
    }

    var statusColor: Color {
        switch self {
        case .disconnected: return .gray
        case .connecting: return Color(red: 1.0, green: 0.76, blue: 0.03) // #FFC107
        case .connected: return Color(red: 0.30, green: 0.69, blue: 0.31) // #4CAF50
        }
    }

    var buttonText: String {
        switch self {
        case .disconnected: return "Подключиться"
        case .connecting: return "Отмена"
        case .connected: return "Отключиться"
        }
    }

    var buttonColor: Color {
        switch self {
        case .disconnected: return Color(red: 0.30, green: 0.69, blue: 0.31) // #4CAF50
        case .connecting: return Color(red: 1.0, green: 0.76, blue: 0.03) // #FFC107
        case .connected: return Color(red: 0.96, green: 0.26, blue: 0.21) // #F44336
        }
    }
}

enum ConnectionMode: String {
    case relay
    case direct
}

struct ContentView: View {
    @AppStorage("callLink") private var callLink = ""
    @AppStorage("serverAddr") private var serverAddr = ""
    @AppStorage("token") private var token = ""
    @AppStorage("connectionMode") private var connectionModeRaw = "relay"
    @AppStorage("numConns") private var numConnsStored = "4"
    @AppStorage("recentIds") private var recentIdsRaw = ""

    @State private var vpnState: VpnState = .disconnected
    @State private var manager: NETunnelProviderManager?
    @State private var activeConns = 0
    @State private var totalConns = 0
    @State private var logLines: [String] = []
    @State private var pollTimer: Timer?
    @State private var copiedToast = false

    // Editing fields (separate from stored to allow editing before connect)
    @State private var editingCallLink = ""
    @State private var editingServerAddr = ""
    @State private var editingToken = ""
    @State private var editingNumConns = "4"

    private let sharedDefaults = UserDefaults(suiteName: "group.com.callvpn.app")

    private var currentMode: ConnectionMode {
        ConnectionMode(rawValue: connectionModeRaw) ?? .relay
    }

    private var parsedId: String {
        parseCallLink(editingCallLink)
    }

    private var hasFullLink: Bool {
        editingCallLink.contains("vk.com/call/join/")
    }

    private var recentIds: [String] {
        recentIdsRaw.split(separator: "\n").map(String.init).filter { !$0.isEmpty }
    }

    private var canConnect: Bool {
        switch currentMode {
        case .relay:
            return !parsedId.isEmpty
        case .direct:
            return !parsedId.isEmpty && !editingServerAddr.isEmpty
        }
    }

    var body: some View {
        ScrollView {
            VStack(spacing: 16) {
                Spacer().frame(height: 32)

                // Title
                Text("CallVPN")
                    .font(.system(size: 32, weight: .bold))

                // Status
                Text(vpnState.statusText)
                    .font(.system(size: 16, weight: .medium))
                    .foregroundColor(vpnState.statusColor)

                // Connection count
                if vpnState != .disconnected && totalConns > 0 {
                    Text("Подключения: \(activeConns) / \(totalConns)")
                        .font(.system(size: 13, design: .monospaced))
                        .foregroundColor(activeConns == totalConns ? .secondary : Color(red: 1.0, green: 0.76, blue: 0.03))
                }

                // Mode picker
                Picker("Mode", selection: $connectionModeRaw) {
                    Text("Relay-to-Relay").tag("relay")
                    Text("Direct").tag("direct")
                }
                .pickerStyle(.segmented)
                .disabled(vpnState != .disconnected)
                .padding(.horizontal)

                Spacer().frame(height: 24)

                // Big round button
                Button(action: handleButtonTap) {
                    Text(vpnState.buttonText)
                        .font(.system(size: 16, weight: .bold))
                        .foregroundColor(.white)
                        .frame(width: 170, height: 170)
                        .background(vpnState.buttonColor)
                        .clipShape(Circle())
                }

                // App version
                Text("v\(Bundle.main.infoDictionary?["CFBundleShortVersionString"] as? String ?? "?")")
                    .font(.system(size: 12))
                    .foregroundColor(.secondary)

                Spacer().frame(height: 24)

                // VK link input
                VStack(alignment: .leading, spacing: 4) {
                    Text("Ссылка VK звонка")
                        .font(.caption)
                        .foregroundColor(.secondary)
                    TextField("https://vk.com/call/join/...", text: $editingCallLink)
                        .textFieldStyle(.roundedBorder)
                        .autocapitalization(.none)
                        .disableAutocorrection(true)
                        .disabled(vpnState != .disconnected)

                    if hasFullLink && !parsedId.isEmpty {
                        Text("ID: \(parsedId)")
                            .font(.caption2)
                            .foregroundColor(.secondary)
                    }
                }
                .padding(.horizontal)

                // Recent call IDs
                if !recentIds.isEmpty && vpnState == .disconnected {
                    VStack(alignment: .leading, spacing: 4) {
                        Text("Недавние")
                            .font(.caption)
                            .foregroundColor(.secondary)

                        ForEach(recentIds, id: \.self) { id in
                            Button {
                                editingCallLink = id
                            } label: {
                                Text(id)
                                    .font(.system(size: 13, design: .monospaced))
                                    .foregroundColor(.secondary)
                                    .frame(maxWidth: .infinity, alignment: .leading)
                                    .padding(.horizontal, 12)
                                    .padding(.vertical, 8)
                                    .background(Color(.systemGray5))
                                    .cornerRadius(6)
                            }
                        }
                    }
                    .padding(.horizontal)
                }

                // Server address input (only in Direct mode)
                if currentMode == .direct {
                    VStack(alignment: .leading, spacing: 4) {
                        Text("Адрес сервера")
                            .font(.caption)
                            .foregroundColor(.secondary)
                        TextField("host:port", text: $editingServerAddr)
                            .textFieldStyle(.roundedBorder)
                            .autocapitalization(.none)
                            .disableAutocorrection(true)
                            .disabled(vpnState != .disconnected)
                    }
                    .padding(.horizontal)
                }

                // Token input
                VStack(alignment: .leading, spacing: 4) {
                    Text("Токен")
                        .font(.caption)
                        .foregroundColor(.secondary)
                    SecureField("Токен авторизации", text: $editingToken)
                        .textFieldStyle(.roundedBorder)
                        .disabled(vpnState != .disconnected)
                }
                .padding(.horizontal)

                // Connections count input
                VStack(alignment: .leading, spacing: 4) {
                    Text("Подключения (1-16)")
                        .font(.caption)
                        .foregroundColor(.secondary)
                    TextField("4", text: $editingNumConns)
                        .textFieldStyle(.roundedBorder)
                        .keyboardType(.numberPad)
                        .disabled(vpnState != .disconnected)
                }
                .padding(.horizontal)

                // Log window (always visible)
                VStack(alignment: .leading, spacing: 4) {
                    Text("Лог")
                        .font(.system(size: 14, weight: .medium))
                        .foregroundColor(.secondary)

                    ZStack {
                        if logLines.isEmpty {
                            Text("Нет записей")
                                .font(.caption)
                                .foregroundColor(.secondary.opacity(0.5))
                                .frame(maxWidth: .infinity, maxHeight: .infinity)
                        } else {
                            ScrollViewReader { proxy in
                                ScrollView {
                                    LazyVStack(alignment: .leading, spacing: 2) {
                                        ForEach(Array(logLines.enumerated()), id: \.offset) { index, line in
                                            Text(line)
                                                .font(.system(size: 11, design: .monospaced))
                                                .foregroundColor(logLineColor(line))
                                                .id(index)
                                        }
                                    }
                                    .padding(8)
                                }
                                .onChange(of: logLines.count) { _ in
                                    if let last = logLines.indices.last {
                                        withAnimation {
                                            proxy.scrollTo(last, anchor: .bottom)
                                        }
                                    }
                                }
                            }
                        }
                    }
                    .frame(height: 300)
                    .frame(maxWidth: .infinity)
                    .background(Color(.systemGray6))
                    .cornerRadius(8)
                    .onTapGesture {
                        if !logLines.isEmpty {
                            UIPasteboard.general.string = logLines.joined(separator: "\n")
                            withAnimation {
                                copiedToast = true
                            }
                            DispatchQueue.main.asyncAfter(deadline: .now() + 1.5) {
                                withAnimation {
                                    copiedToast = false
                                }
                            }
                        }
                    }
                    .overlay(alignment: .bottom) {
                        if copiedToast {
                            Text("Логи скопированы")
                                .font(.caption)
                                .foregroundColor(.white)
                                .padding(.horizontal, 12)
                                .padding(.vertical, 6)
                                .background(Color.black.opacity(0.7))
                                .cornerRadius(8)
                                .padding(.bottom, 8)
                                .transition(.opacity)
                        }
                    }
                }
                .padding(.horizontal)

                Spacer()
            }
            .padding()
        }
        .onAppear {
            editingCallLink = callLink
            editingServerAddr = serverAddr
            editingToken = token
            editingNumConns = numConnsStored
            loadManager()
            observeVPNStatus()
            startPolling()
        }
        .onDisappear {
            stopPolling()
        }
    }

    // MARK: - Log line coloring

    private func logLineColor(_ line: String) -> Color {
        if line.contains("level=ERROR") || line.hasPrefix("ERROR:") {
            return Color(red: 0.94, green: 0.33, blue: 0.31) // #EF5350
        }
        if line.contains("level=WARN") {
            return Color(red: 1.0, green: 0.76, blue: 0.03) // #FFC107
        }
        return .secondary
    }

    // MARK: - Button action

    private func handleButtonTap() {
        switch vpnState {
        case .disconnected:
            guard canConnect else { return }
            let conns = max(1, min(16, Int(editingNumConns) ?? 4))
            // Save settings
            callLink = editingCallLink
            serverAddr = editingServerAddr
            token = editingToken
            numConnsStored = String(conns)
            editingNumConns = String(conns)
            // Save to recent IDs
            saveRecentId(parsedId)
            startVPN(numConns: conns)
        case .connecting:
            stopVPN()
        case .connected:
            stopVPN()
        }
    }

    // MARK: - Recent IDs

    private func saveRecentId(_ id: String) {
        var ids = recentIds.filter { $0 != id }
        ids.insert(id, at: 0)
        let capped = Array(ids.prefix(5))
        recentIdsRaw = capped.joined(separator: "\n")
    }

    // MARK: - VPN management

    private func loadManager() {
        NETunnelProviderManager.loadAllFromPreferences { managers, error in
            if let existing = managers?.first {
                self.manager = existing
            }
        }
    }

    private func observeVPNStatus() {
        NotificationCenter.default.addObserver(
            forName: .NEVPNStatusDidChange,
            object: nil,
            queue: .main
        ) { notification in
            guard let connection = notification.object as? NEVPNConnection else { return }
            switch connection.status {
            case .connected:
                vpnState = .connected
            case .connecting, .reasserting:
                vpnState = .connecting
            case .disconnected, .invalid:
                vpnState = .disconnected
                activeConns = 0
                totalConns = 0
            case .disconnecting:
                vpnState = .connecting
            @unknown default:
                break
            }
        }
    }

    // MARK: - Polling (logs + connection state)

    private func startPolling() {
        stopPolling()
        pollTimer = Timer.scheduledTimer(withTimeInterval: 0.5, repeats: true) { _ in
            // Poll logs
            if let logs = sharedDefaults?.string(forKey: "vpn_logs"), !logs.isEmpty {
                let newLines = logs.split(separator: "\n").map(String.init)
                DispatchQueue.main.async {
                    logLines = Array((logLines + newLines).suffix(500))
                }
                sharedDefaults?.removeObject(forKey: "vpn_logs")
            }

            // Poll connection state
            let isConnected = sharedDefaults?.bool(forKey: "vpn_is_connected") ?? false
            let active = sharedDefaults?.integer(forKey: "vpn_active_conns") ?? 0
            let total = sharedDefaults?.integer(forKey: "vpn_total_conns") ?? 0

            DispatchQueue.main.async {
                activeConns = active
                totalConns = total

                // Sync connection state from tunnel (handles reconnecting)
                if vpnState == .connected && !isConnected {
                    vpnState = .connecting
                } else if vpnState == .connecting && isConnected {
                    // Only upgrade to connected if NEVPNStatus also says connected
                    if let mgr = manager,
                       mgr.connection.status == .connected {
                        vpnState = .connected
                    }
                }
            }
        }
    }

    private func stopPolling() {
        pollTimer?.invalidate()
        pollTimer = nil
    }

    private func startVPN(numConns: Int) {
        vpnState = .connecting
        logLines = []

        let effectiveServerAddr = currentMode == .relay ? "" : editingServerAddr
        let displayAddr = currentMode == .relay ? "relay.vk.com" : editingServerAddr

        let configureAndStart: (NETunnelProviderManager) -> Void = { mgr in
            let proto = NETunnelProviderProtocol()
            proto.providerBundleIdentifier = "com.callvpn.app.PacketTunnel"
            proto.serverAddress = displayAddr
            proto.providerConfiguration = [
                "callLink": parsedId,
                "serverAddr": effectiveServerAddr,
                "numConns": numConns,
                "token": editingToken
            ]
            mgr.protocolConfiguration = proto
            mgr.localizedDescription = "CallVPN"
            mgr.isEnabled = true

            mgr.saveToPreferences { error in
                if let error = error {
                    NSLog("CallVPN: save error: \(error)")
                    vpnState = .disconnected
                    return
                }

                mgr.loadFromPreferences { error in
                    if let error = error {
                        NSLog("CallVPN: load error: \(error)")
                        vpnState = .disconnected
                        return
                    }

                    do {
                        try (mgr.connection as? NETunnelProviderSession)?.startTunnel()
                        self.manager = mgr
                    } catch {
                        NSLog("CallVPN: start error: \(error)")
                        vpnState = .disconnected
                    }
                }
            }
        }

        if let existing = manager {
            configureAndStart(existing)
        } else {
            let mgr = NETunnelProviderManager()
            configureAndStart(mgr)
        }
    }

    private func stopVPN() {
        manager?.connection.stopVPNTunnel()
    }
}

private func parseCallLink(_ input: String) -> String {
    if let range = input.range(of: #"vk\.com/call/join/([A-Za-z0-9_-]+)"#, options: .regularExpression) {
        let fullMatch = String(input[range])
        if let slashRange = fullMatch.range(of: "join/") {
            return String(fullMatch[slashRange.upperBound...])
        }
    }
    return input.trimmingCharacters(in: .whitespacesAndNewlines)
}
