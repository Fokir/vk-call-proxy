import NetworkExtension
import Network
import Bind // gomobile: package bind → module Bind

class PacketTunnelProvider: NEPacketTunnelProvider {

    private var tunnel: BindTunnel?
    private var logTimer: DispatchSourceTimer?
    private var stateTimer: DispatchSourceTimer?
    private var pathMonitor: NWPathMonitor?
    private let sharedDefaults = UserDefaults(suiteName: "group.com.callvpn.app")

    override func startTunnel(options: [String: NSObject]?, completionHandler: @escaping (Error?) -> Void) {
        guard let proto = protocolConfiguration as? NETunnelProviderProtocol,
              let config = proto.providerConfiguration,
              let callLink = config["callLink"] as? String else {
            completionHandler(makeError("Missing configuration"))
            return
        }

        let serverAddr = (config["serverAddr"] as? String) ?? ""
        let numConns = (config["numConns"] as? Int) ?? 4
        let token = (config["token"] as? String) ?? ""

        let tunnelRemoteAddr = serverAddr.isEmpty ? "relay.vk.com" : serverAddr

        let settings = NEPacketTunnelNetworkSettings(tunnelRemoteAddress: tunnelRemoteAddr)

        // IPv4
        let ipv4 = NEIPv4Settings(addresses: ["10.0.0.2"], subnetMasks: ["255.255.255.0"])
        ipv4.includedRoutes = [NEIPv4Route.default()]
        settings.ipv4Settings = ipv4

        // IPv6
        let ipv6 = NEIPv6Settings(addresses: ["fd00::2"], networkPrefixLengths: [128])
        ipv6.includedRoutes = [NEIPv6Route.default()]
        settings.ipv6Settings = ipv6

        // DNS
        settings.dnsSettings = NEDNSSettings(servers: ["8.8.8.8", "8.8.4.4"])
        settings.mtu = 1280

        setTunnelNetworkSettings(settings) { [weak self] error in
            if let error = error {
                completionHandler(error)
                return
            }

            // Build Go tunnel config.
            let cfg = BindTunnelConfig()
            cfg.callLink = callLink
            cfg.serverAddr = serverAddr
            cfg.numConns = numConns
            cfg.useTCP = true
            cfg.token = token

            guard let t = BindNewTunnel() else {
                completionHandler(self?.makeError("Failed to create tunnel") ?? NSError())
                return
            }

            // gomobile bridges (error) return as Swift throws.
            do {
                try t.start(cfg)
            } catch {
                // Read any Go-side logs emitted before the error.
                let goLogs = t.readLogs()
                if !goLogs.isEmpty {
                    self?.appendLogs(goLogs)
                }
                completionHandler(error)
                return
            }

            self?.tunnel = t
            self?.startLogForwarding()
            self?.startStateForwarding()
            self?.startPacketForwarding()
            self?.startPathMonitor()
            completionHandler(nil)
        }
    }

    // MARK: - Log forwarding

    private func startLogForwarding() {
        let timer = DispatchSource.makeTimerSource(queue: DispatchQueue.global(qos: .utility))
        timer.schedule(deadline: .now(), repeating: .milliseconds(500))
        timer.setEventHandler { [weak self] in
            guard let self = self, let tunnel = self.tunnel else { return }
            // ReadLogs atomically reads and clears the buffer (ReadAndClear in Go).
            let logs = tunnel.readLogs()
            if !logs.isEmpty {
                self.appendLogs(logs)
            }
        }
        timer.resume()
        logTimer = timer
    }

    private func appendLogs(_ text: String) {
        let existing = sharedDefaults?.string(forKey: "vpn_logs") ?? ""
        let combined = existing.isEmpty ? text : existing + "\n" + text
        sharedDefaults?.set(combined, forKey: "vpn_logs")
    }

    // MARK: - Connection state forwarding

    private func startStateForwarding() {
        let timer = DispatchSource.makeTimerSource(queue: DispatchQueue.global(qos: .utility))
        timer.schedule(deadline: .now(), repeating: .milliseconds(500))
        timer.setEventHandler { [weak self] in
            guard let self = self, let tunnel = self.tunnel else { return }
            self.sharedDefaults?.set(tunnel.isConnected(), forKey: "vpn_is_connected")
            self.sharedDefaults?.set(tunnel.activeConns(), forKey: "vpn_active_conns")
            self.sharedDefaults?.set(tunnel.totalConns(), forKey: "vpn_total_conns")
        }
        timer.resume()
        stateTimer = timer
    }

    // MARK: - Packet forwarding

    private func startPacketForwarding() {
        // TUN → tunnel
        readPacketsFromTUN()

        // tunnel → TUN
        DispatchQueue.global(qos: .userInteractive).async { [weak self] in
            while self?.tunnel?.isRunning() == true {
                guard let self = self, let tunnel = self.tunnel else { break }

                // gomobile: ReadPacketData() ([]byte, error) → throws -> Data
                let data: Data
                do {
                    data = try tunnel.readPacketData()
                } catch {
                    // Tunnel stopped or reconnecting — brief pause and retry.
                    usleep(10_000) // 10ms
                    continue
                }

                if data.isEmpty { continue }

                // Detect IP version from first nibble.
                let version = data[data.startIndex] >> 4
                let proto: NSNumber
                if version == 6 {
                    proto = NSNumber(value: AF_INET6)
                } else {
                    proto = NSNumber(value: AF_INET)
                }

                self.packetFlow.writePackets([data], withProtocols: [proto])
            }
        }
    }

    private func readPacketsFromTUN() {
        packetFlow.readPackets { [weak self] packets, protocols in
            guard let self = self, let tunnel = self.tunnel else { return }

            for packet in packets {
                // gomobile: WritePacket(data []byte) error → throws
                try? tunnel.writePacket(packet)
            }

            // Continue reading (recursive call for NEPacketTunnelProvider pattern).
            self.readPacketsFromTUN()
        }
    }

    // MARK: - Network path monitor

    private func startPathMonitor() {
        let monitor = NWPathMonitor()
        monitor.pathUpdateHandler = { [weak self] _ in
            self?.tunnel?.onNetworkChanged()
        }
        monitor.start(queue: DispatchQueue.global(qos: .utility))
        pathMonitor = monitor
    }

    // MARK: - Stop

    override func stopTunnel(with reason: NEProviderStopReason, completionHandler: @escaping () -> Void) {
        pathMonitor?.cancel()
        pathMonitor = nil
        logTimer?.cancel()
        logTimer = nil
        stateTimer?.cancel()
        stateTimer = nil
        sharedDefaults?.removeObject(forKey: "vpn_logs")
        sharedDefaults?.removeObject(forKey: "vpn_is_connected")
        sharedDefaults?.removeObject(forKey: "vpn_active_conns")
        sharedDefaults?.removeObject(forKey: "vpn_total_conns")
        tunnel?.stop()
        tunnel = nil
        completionHandler()
    }

    private func makeError(_ message: String) -> NSError {
        NSError(domain: "CallVPN", code: 1, userInfo: [NSLocalizedDescriptionKey: message])
    }
}
