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

struct ContentView: View {
    @State private var callLink = ""
    @State private var serverAddr = ""
    @State private var isConnected = false
    @State private var statusText = "Disconnected"

    var body: some View {
        VStack(spacing: 20) {
            Text("CallVPN")
                .font(.largeTitle)
                .fontWeight(.bold)

            TextField("VK Call Link ID", text: $callLink)
                .textFieldStyle(.roundedBorder)
                .autocapitalization(.none)

            TextField("Server Address (host:port)", text: $serverAddr)
                .textFieldStyle(.roundedBorder)
                .autocapitalization(.none)

            Text(statusText)
                .foregroundColor(isConnected ? .green : .secondary)

            Button(action: toggleVPN) {
                Text(isConnected ? "Disconnect" : "Connect")
                    .frame(maxWidth: .infinity)
                    .padding()
                    .background(isConnected ? Color.red : Color.blue)
                    .foregroundColor(.white)
                    .cornerRadius(10)
            }
        }
        .padding()
    }

    private func toggleVPN() {
        if isConnected {
            stopVPN()
        } else {
            startVPN()
        }
    }

    private func startVPN() {
        guard !callLink.isEmpty, !serverAddr.isEmpty else { return }

        statusText = "Connecting..."

        let manager = NETunnelProviderManager()
        let proto = NETunnelProviderProtocol()
        proto.providerBundleIdentifier = "com.callvpn.app.PacketTunnel"
        proto.serverAddress = serverAddr
        proto.providerConfiguration = [
            "callLink": callLink,
            "serverAddr": serverAddr,
            "numConns": 4
        ]
        manager.protocolConfiguration = proto
        manager.localizedDescription = "CallVPN"
        manager.isEnabled = true

        manager.saveToPreferences { error in
            if let error = error {
                statusText = "Error: \(error.localizedDescription)"
                return
            }

            manager.loadFromPreferences { error in
                if let error = error {
                    statusText = "Error: \(error.localizedDescription)"
                    return
                }

                do {
                    try (manager.connection as? NETunnelProviderSession)?.startTunnel()
                    isConnected = true
                    statusText = "Connected"
                } catch {
                    statusText = "Error: \(error.localizedDescription)"
                }
            }
        }
    }

    private func stopVPN() {
        let manager = NETunnelProviderManager()
        manager.connection.stopVPNTunnel()
        isConnected = false
        statusText = "Disconnected"
    }
}
