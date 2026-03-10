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
        case .connecting: return Color(red: 1.0, green: 0.76, blue: 0.03)
        case .connected: return Color(red: 0.30, green: 0.69, blue: 0.31)
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
        case .disconnected: return Color(red: 0.30, green: 0.69, blue: 0.31)
        case .connecting: return Color(red: 1.0, green: 0.76, blue: 0.03)
        case .connected: return Color(red: 0.96, green: 0.26, blue: 0.21)
        }
    }
}

struct ContentView: View {
    @StateObject private var profileManager = ProfileManager()

    @State private var vpnState: VpnState = .disconnected
    @State private var manager: NETunnelProviderManager?
    @State private var activeConns = 0
    @State private var totalConns = 0
    @State private var logLines: [String] = []
    @State private var pollTimer: Timer?
    @State private var copiedToast = false
    @State private var showEditor = false
    @State private var editingProfile: Profile?

    private let sharedDefaults = UserDefaults(suiteName: "group.com.callvpn.app")

    private var activeProfile: Profile? {
        profileManager.activeProfile
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

                Spacer().frame(height: 8)

                // Profile badges
                profileBadgesSection

                Spacer().frame(height: 24)

                // Big round button
                Button(action: handleButtonTap) {
                    Text(vpnState.buttonText)
                        .font(.system(size: 16, weight: .bold))
                        .foregroundColor(.white)
                        .frame(width: 170, height: 170)
                        .background(buttonBackgroundColor)
                        .clipShape(Circle())
                }
                .disabled(vpnState == .disconnected && activeProfile == nil)

                // App version
                Text("v\(Bundle.main.infoDictionary?["CFBundleShortVersionString"] as? String ?? "?")")
                    .font(.system(size: 12))
                    .foregroundColor(.secondary)

                Spacer().frame(height: 24)

                // Log window
                logSection

                Spacer()
            }
            .padding()
        }
        .onAppear {
            loadManager()
            observeVPNStatus()
            startPolling()
        }
        .onDisappear {
            stopPolling()
        }
        .sheet(isPresented: $showEditor) {
            ProfileEditorView(
                profile: editingProfile,
                onSave: { saved in
                    profileManager.saveProfile(saved)
                    // If no active profile, make this one active
                    if profileManager.activeProfileId == nil {
                        profileManager.setActiveProfile(saved.id)
                    }
                    // If edited profile is active and connected, reconnect
                    if saved.id == profileManager.activeProfileId && vpnState != .disconnected {
                        stopVPN()
                        DispatchQueue.main.asyncAfter(deadline: .now() + 0.5) {
                            connectProfile(saved)
                        }
                    }
                    showEditor = false
                    editingProfile = nil
                },
                onDelete: editingProfile != nil ? { id in
                    let wasActive = id == profileManager.activeProfileId
                    if wasActive && vpnState != .disconnected {
                        stopVPN()
                    }
                    profileManager.deleteProfile(id)
                    showEditor = false
                    editingProfile = nil
                } : nil,
                onDismiss: {
                    showEditor = false
                    editingProfile = nil
                }
            )
        }
    }

    private var buttonBackgroundColor: Color {
        if vpnState == .disconnected && activeProfile == nil {
            return .gray
        }
        return vpnState.buttonColor
    }

    // MARK: - Profile Badges

    private var profileBadgesSection: some View {
        FlowLayout(spacing: 8) {
            ForEach(profileManager.profiles) { profile in
                let isActive = profile.id == profileManager.activeProfileId
                ProfileBadgeView(
                    profile: profile,
                    isActive: isActive,
                    onSelect: {
                        if !isActive {
                            if vpnState != .disconnected {
                                stopVPN()
                            }
                            profileManager.setActiveProfile(profile.id)
                            DispatchQueue.main.asyncAfter(deadline: .now() + 0.3) {
                                connectProfile(profile)
                            }
                        }
                    },
                    onEdit: {
                        editingProfile = profile
                        showEditor = true
                    }
                )
            }

            // Add button
            Button {
                editingProfile = nil
                showEditor = true
            } label: {
                HStack(spacing: 6) {
                    Image(systemName: "plus")
                        .font(.system(size: 14))
                    Text("Добавить")
                        .font(.system(size: 13))
                }
                .foregroundColor(.secondary)
                .padding(.horizontal, 16)
                .frame(height: 40)
                .background(
                    RoundedRectangle(cornerRadius: 20)
                        .stroke(Color.secondary.opacity(0.3), lineWidth: 1)
                )
            }
        }
        .padding(.horizontal)
    }

    // MARK: - Log section

    private var logSection: some View {
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
                    withAnimation { copiedToast = true }
                    DispatchQueue.main.asyncAfter(deadline: .now() + 1.5) {
                        withAnimation { copiedToast = false }
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
    }

    // MARK: - Log line coloring

    private func logLineColor(_ line: String) -> Color {
        if line.contains("level=ERROR") || line.hasPrefix("ERROR:") {
            return Color(red: 0.94, green: 0.33, blue: 0.31)
        }
        if line.contains("level=WARN") {
            return Color(red: 1.0, green: 0.76, blue: 0.03)
        }
        return .secondary
    }

    // MARK: - Button action

    private func handleButtonTap() {
        switch vpnState {
        case .disconnected:
            guard let profile = activeProfile else { return }
            connectProfile(profile)
        case .connecting:
            stopVPN()
        case .connected:
            stopVPN()
        }
    }

    private func connectProfile(_ profile: Profile) {
        let callLink = parseCallLink(profile.callLink)
        guard !callLink.isEmpty else { return }
        let serverAddr = profile.connectionMode == "direct" ? profile.serverAddr : ""
        let conns = max(1, min(16, profile.numConns))
        startVPN(callLink: callLink, serverAddr: serverAddr, token: profile.token, numConns: conns)
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

    // MARK: - Polling

    private func startPolling() {
        stopPolling()
        pollTimer = Timer.scheduledTimer(withTimeInterval: 0.5, repeats: true) { _ in
            if let logs = sharedDefaults?.string(forKey: "vpn_logs"), !logs.isEmpty {
                let newLines = logs.split(separator: "\n").map(String.init)
                DispatchQueue.main.async {
                    logLines = Array((logLines + newLines).suffix(500))
                }
                sharedDefaults?.removeObject(forKey: "vpn_logs")
            }

            let isConnected = sharedDefaults?.bool(forKey: "vpn_is_connected") ?? false
            let active = sharedDefaults?.integer(forKey: "vpn_active_conns") ?? 0
            let total = sharedDefaults?.integer(forKey: "vpn_total_conns") ?? 0

            DispatchQueue.main.async {
                activeConns = active
                totalConns = total

                if vpnState == .connected && !isConnected {
                    vpnState = .connecting
                } else if vpnState == .connecting && isConnected {
                    if let mgr = manager, mgr.connection.status == .connected {
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

    private func startVPN(callLink: String, serverAddr: String, token: String, numConns: Int) {
        vpnState = .connecting
        logLines = []

        let effectiveServerAddr = serverAddr
        let displayAddr = serverAddr.isEmpty ? "relay.vk.com" : serverAddr

        let configureAndStart: (NETunnelProviderManager) -> Void = { mgr in
            let proto = NETunnelProviderProtocol()
            proto.providerBundleIdentifier = "com.callvpn.app.PacketTunnel"
            proto.serverAddress = displayAddr
            proto.providerConfiguration = [
                "callLink": callLink,
                "serverAddr": effectiveServerAddr,
                "numConns": numConns,
                "token": token
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

// MARK: - Profile Badge View

struct ProfileBadgeView: View {
    let profile: Profile
    let isActive: Bool
    let onSelect: () -> Void
    let onEdit: () -> Void

    var body: some View {
        HStack(spacing: 6) {
            // Provider icon
            Text(profile.isTelemostLink ? "Я" : "VK")
                .font(.system(size: 12, weight: .bold))
                .foregroundColor(isActive ? .white : providerColor)

            // Profile name
            Text(profile.name.isEmpty ? "Без имени" : profile.name)
                .font(.system(size: 13, weight: .medium))
                .foregroundColor(isActive ? .white : .primary)

            // Edit button
            Button(action: onEdit) {
                Image(systemName: "pencil")
                    .font(.system(size: 12))
                    .foregroundColor(isActive ? .white.opacity(0.7) : .secondary)
            }
        }
        .padding(.horizontal, 12)
        .frame(height: 40)
        .background(
            RoundedRectangle(cornerRadius: 20)
                .fill(isActive ? Color(red: 0.30, green: 0.69, blue: 0.31) : Color(.systemGray5))
        )
        .overlay(
            RoundedRectangle(cornerRadius: 20)
                .stroke(isActive ? Color(red: 0.30, green: 0.69, blue: 0.31) : Color.secondary.opacity(0.3), lineWidth: 1)
        )
        .onTapGesture { onSelect() }
    }

    private var providerColor: Color {
        profile.isTelemostLink ? .red : Color(red: 0, green: 0.47, blue: 1.0)
    }
}

// MARK: - Profile Editor View

struct ProfileEditorView: View {
    let profile: Profile?
    let onSave: (Profile) -> Void
    let onDelete: ((UUID) -> Void)?
    let onDismiss: () -> Void

    @State private var name: String
    @State private var connectionMode: String
    @State private var callLink: String
    @State private var serverAddr: String
    @State private var token: String
    @State private var numConns: String
    @State private var showDeleteConfirm = false

    private let base: Profile

    init(profile: Profile?, onSave: @escaping (Profile) -> Void, onDelete: ((UUID) -> Void)?, onDismiss: @escaping () -> Void) {
        self.profile = profile
        self.onSave = onSave
        self.onDelete = onDelete
        self.onDismiss = onDismiss

        let b = profile ?? Profile()
        self.base = b
        _name = State(initialValue: b.name)
        _connectionMode = State(initialValue: b.connectionMode)
        _callLink = State(initialValue: b.callLink)
        _serverAddr = State(initialValue: b.serverAddr)
        _token = State(initialValue: b.token)
        _numConns = State(initialValue: String(b.numConns))
    }

    var body: some View {
        NavigationView {
            Form {
                Section {
                    TextField("Имя профиля", text: $name)
                        .onChange(of: name) { newValue in
                            if newValue.count > 20 {
                                name = String(newValue.prefix(20))
                            }
                        }
                }

                Section(header: Text("Тип подключения")) {
                    Picker("Режим", selection: $connectionMode) {
                        Text("Relay-to-Relay").tag("relay")
                        Text("Direct").tag("direct")
                    }
                    .pickerStyle(.segmented)
                }

                Section(header: Text("Подключение")) {
                    TextField("https://vk.com/call/join/...", text: $callLink)
                        .autocapitalization(.none)
                        .disableAutocorrection(true)

                    if connectionMode == "direct" {
                        TextField("host:port", text: $serverAddr)
                            .autocapitalization(.none)
                            .disableAutocorrection(true)
                    }

                    SecureField("Токен авторизации", text: $token)

                    TextField("Подключения (1-16)", text: $numConns)
                        .keyboardType(.numberPad)
                }

                if profile != nil, let onDelete = onDelete {
                    Section {
                        Button("Удалить профиль") {
                            showDeleteConfirm = true
                        }
                        .foregroundColor(.red)
                        .frame(maxWidth: .infinity, alignment: .center)
                    }
                }
            }
            .navigationTitle(profile == nil ? "Новый профиль" : "Редактирование")
            .navigationBarTitleDisplayMode(.inline)
            .toolbar {
                ToolbarItem(placement: .cancellationAction) {
                    Button("Отмена") { onDismiss() }
                }
                ToolbarItem(placement: .confirmationAction) {
                    Button("Сохранить") {
                        let conns = max(1, min(16, Int(numConns) ?? 4))
                        let saved = Profile(
                            id: base.id,
                            name: name.trimmingCharacters(in: .whitespaces),
                            connectionMode: connectionMode,
                            callLink: callLink.trimmingCharacters(in: .whitespaces),
                            serverAddr: serverAddr.trimmingCharacters(in: .whitespaces),
                            token: token,
                            numConns: conns
                        )
                        onSave(saved)
                    }
                    .fontWeight(.bold)
                }
            }
            .alert("Удалить профиль?", isPresented: $showDeleteConfirm) {
                Button("Удалить", role: .destructive) {
                    if let p = profile {
                        onDelete?(p.id)
                    }
                }
                Button("Отмена", role: .cancel) {}
            } message: {
                Text("Профиль \"\(profile?.name.isEmpty == false ? profile!.name : "Без имени")\" будет удалён.")
            }
        }
    }
}

// MARK: - FlowLayout (wrap badges to new lines)

struct FlowLayout: Layout {
    var spacing: CGFloat = 8

    func sizeThatFits(proposal: ProposedViewSize, subviews: Subviews, cache: inout ()) -> CGSize {
        let result = arrange(proposal: proposal, subviews: subviews)
        return result.size
    }

    func placeSubviews(in bounds: CGRect, proposal: ProposedViewSize, subviews: Subviews, cache: inout ()) {
        let result = arrange(proposal: proposal, subviews: subviews)
        for (index, position) in result.positions.enumerated() {
            subviews[index].place(
                at: CGPoint(x: bounds.minX + position.x, y: bounds.minY + position.y),
                proposal: .unspecified
            )
        }
    }

    private func arrange(proposal: ProposedViewSize, subviews: Subviews) -> ArrangeResult {
        let maxWidth = proposal.width ?? .infinity
        var positions: [CGPoint] = []
        var x: CGFloat = 0
        var y: CGFloat = 0
        var rowHeight: CGFloat = 0
        var totalHeight: CGFloat = 0

        for subview in subviews {
            let size = subview.sizeThatFits(.unspecified)
            if x + size.width > maxWidth && x > 0 {
                x = 0
                y += rowHeight + spacing
                rowHeight = 0
            }
            positions.append(CGPoint(x: x, y: y))
            rowHeight = max(rowHeight, size.height)
            x += size.width + spacing
            totalHeight = y + rowHeight
        }

        return ArrangeResult(
            size: CGSize(width: maxWidth, height: totalHeight),
            positions: positions
        )
    }

    struct ArrangeResult {
        var size: CGSize
        var positions: [CGPoint]
    }
}

// MARK: - Parse call link

private func parseCallLink(_ input: String) -> String {
    if let range = input.range(of: #"vk\.com/call/join/([A-Za-z0-9_-]+)"#, options: .regularExpression) {
        let fullMatch = String(input[range])
        if let slashRange = fullMatch.range(of: "join/") {
            return String(fullMatch[slashRange.upperBound...])
        }
    }

    if let range = input.range(of: #"telemost\.yandex\.\w+/j/(\d+)"#, options: .regularExpression) {
        let fullMatch = String(input[range])
        if let slashRange = fullMatch.range(of: "/j/") {
            return String(fullMatch[slashRange.upperBound...])
        }
    }

    return input.trimmingCharacters(in: .whitespacesAndNewlines)
}
