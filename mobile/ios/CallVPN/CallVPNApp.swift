import SwiftUI
import NetworkExtension

@main
struct CallVPNApp: App {
    var body: some Scene {
        WindowGroup {
            ContentView()
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
        case .connecting: return .yellow
        case .connected: return .green
        }
    }

    var buttonText: String {
        switch self {
        case .disconnected: return "Подключиться"
        case .connecting: return "Подключение..."
        case .connected: return "Отключиться"
        }
    }

    var buttonColor: Color {
        switch self {
        case .disconnected: return .green
        case .connecting: return .yellow
        case .connected: return .red
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
    @State private var vpnState: VpnState = .disconnected
    @State private var manager: NETunnelProviderManager?
    @State private var showChangeAlert = false
    @State private var pendingChange: (() -> Void)?
    @State private var editingCallLink = ""
    @State private var editingServerAddr = ""
    @State private var editingToken = ""
    @State private var logLines: [String] = []
    @State private var logPollTimer: Timer?

    private var currentMode: ConnectionMode {
        ConnectionMode(rawValue: connectionModeRaw) ?? .relay
    }

    private var parsedId: String {
        parseCallLink(editingCallLink)
    }

    private var hasFullLink: Bool {
        editingCallLink.contains("vk.com/call/join/")
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
                .disabled(vpnState == .connecting)

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
                        .onChange(of: editingCallLink) { newValue in
                            handleFieldChange(saved: callLink, new: newValue) {
                                editingCallLink = newValue
                            }
                        }

                    if hasFullLink && !parsedId.isEmpty {
                        Text("ID: \(parsedId)")
                            .font(.caption2)
                            .foregroundColor(.secondary)
                    }
                }
                .padding(.horizontal)

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
                            .onChange(of: editingServerAddr) { newValue in
                                handleFieldChange(saved: serverAddr, new: newValue) {
                                    editingServerAddr = newValue
                                }
                            }
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

                // Log window
                if !logLines.isEmpty {
                    VStack(alignment: .leading, spacing: 4) {
                        Text("Лог")
                            .font(.system(size: 14, weight: .medium))
                            .foregroundColor(.secondary)

                        ScrollViewReader { proxy in
                            ScrollView {
                                LazyVStack(alignment: .leading, spacing: 2) {
                                    ForEach(Array(logLines.enumerated()), id: \.offset) { index, line in
                                        Text(line)
                                            .font(.system(size: 11, design: .monospaced))
                                            .foregroundColor(.secondary)
                                            .id(index)
                                    }
                                }
                                .padding(8)
                            }
                            .frame(height: 150)
                            .background(Color(.systemGray6))
                            .cornerRadius(8)
                            .onChange(of: logLines.count) { _ in
                                if let last = logLines.indices.last {
                                    proxy.scrollTo(last, anchor: .bottom)
                                }
                            }
                        }
                    }
                    .padding(.horizontal)
                }

                Spacer()
            }
            .padding()
        }
        .alert("Изменить значение?", isPresented: $showChangeAlert) {
            Button("Изменить") {
                pendingChange?()
                pendingChange = nil
            }
            Button("Отмена", role: .cancel) {
                pendingChange = nil
            }
        } message: {
            Text("Сохранённое значение будет заменено.")
        }
        .onAppear {
            editingCallLink = callLink
            editingServerAddr = serverAddr
            editingToken = token
            loadManager()
            observeVPNStatus()
            startLogPolling()
        }
        .onDisappear {
            stopLogPolling()
        }
    }

    private func handleFieldChange(saved: String, new: String, apply: @escaping () -> Void) {
        // Show confirmation only if there's a saved non-empty value being changed
    }

    private func handleButtonTap() {
        switch vpnState {
        case .disconnected:
            guard canConnect else { return }
            callLink = editingCallLink
            serverAddr = editingServerAddr
            token = editingToken
            startVPN()
        case .connected:
            stopVPN()
        case .connecting:
            break
        }
    }

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
                logLines = []
                stopLogPolling()
            case .disconnecting:
                vpnState = .connecting
            @unknown default:
                break
            }
        }
    }

    private func startLogPolling() {
        stopLogPolling()
        let defaults = UserDefaults(suiteName: "group.com.callvpn.app")
        logPollTimer = Timer.scheduledTimer(withTimeInterval: 0.5, repeats: true) { _ in
            guard let logs = defaults?.string(forKey: "vpn_logs"), !logs.isEmpty else { return }
            let newLines = logs.split(separator: "\n").map(String.init)
            DispatchQueue.main.async {
                logLines = (logLines + newLines).suffix(20)
            }
            defaults?.removeObject(forKey: "vpn_logs")
        }
    }

    private func stopLogPolling() {
        logPollTimer?.invalidate()
        logPollTimer = nil
    }

    private func startVPN() {
        vpnState = .connecting

        let effectiveServerAddr = currentMode == .relay ? "" : editingServerAddr
        let displayAddr = currentMode == .relay ? "relay.vk.com" : editingServerAddr

        let configureAndStart: (NETunnelProviderManager) -> Void = { mgr in
            let proto = NETunnelProviderProtocol()
            proto.providerBundleIdentifier = "com.callvpn.app.PacketTunnel"
            proto.serverAddress = displayAddr
            proto.providerConfiguration = [
                "callLink": parsedId,
                "serverAddr": effectiveServerAddr,
                "numConns": 4,
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
                        startLogPolling()
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
