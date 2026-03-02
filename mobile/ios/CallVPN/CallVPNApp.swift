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

struct ContentView: View {
    @AppStorage("callLink") private var callLink = ""
    @AppStorage("serverAddr") private var serverAddr = ""
    @AppStorage("token") private var token = ""
    @State private var vpnState: VpnState = .disconnected
    @State private var manager: NETunnelProviderManager?
    @State private var showChangeAlert = false
    @State private var pendingChange: (() -> Void)?
    @State private var editingCallLink = ""
    @State private var editingServerAddr = ""
    @State private var editingToken = ""

    private var parsedId: String {
        parseCallLink(editingCallLink)
    }

    private var hasFullLink: Bool {
        editingCallLink.contains("vk.com/call/join/")
    }

    var body: some View {
        VStack(spacing: 16) {
            Spacer().frame(height: 32)

            // Title
            Text("CallVPN")
                .font(.system(size: 32, weight: .bold))

            // Status
            Text(vpnState.statusText)
                .font(.system(size: 16, weight: .medium))
                .foregroundColor(vpnState.statusColor)

            Spacer().frame(height: 24)

            // Big round button
            Button(action: handleButtonTap) {
                Text(vpnState.buttonText)
                    .font(.system(size: 14, weight: .bold))
                    .foregroundColor(.white)
                    .frame(width: 140, height: 140)
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

            // Server address input
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

            Spacer()
        }
        .padding()
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
        }
    }

    private func handleFieldChange(saved: String, new: String, apply: @escaping () -> Void) {
        // Show confirmation only if there's a saved non-empty value being changed
        // The onChange fires naturally, so we only intercept when saved != empty
        // and the user is starting to modify it
    }

    private func handleButtonTap() {
        switch vpnState {
        case .disconnected:
            guard !parsedId.isEmpty, !editingServerAddr.isEmpty else { return }
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
            case .disconnecting:
                vpnState = .connecting
            @unknown default:
                break
            }
        }
    }

    private func startVPN() {
        vpnState = .connecting

        let configureAndStart: (NETunnelProviderManager) -> Void = { mgr in
            let proto = NETunnelProviderProtocol()
            proto.providerBundleIdentifier = "com.callvpn.app.PacketTunnel"
            proto.serverAddress = editingServerAddr
            proto.providerConfiguration = [
                "callLink": parsedId,
                "serverAddr": editingServerAddr,
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
