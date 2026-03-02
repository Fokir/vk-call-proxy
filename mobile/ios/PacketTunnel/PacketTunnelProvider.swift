import NetworkExtension
import Bind // gomobile generated framework

class PacketTunnelProvider: NEPacketTunnelProvider {

    private var tunnel: BindTunnel?
    private var logTimer: DispatchSourceTimer?
    private let logDefaults = UserDefaults(suiteName: "group.com.callvpn.app")

    override func startTunnel(options: [String: NSObject]?, completionHandler: @escaping (Error?) -> Void) {
        guard let proto = protocolConfiguration as? NETunnelProviderProtocol,
              let config = proto.providerConfiguration,
              let callLink = config["callLink"] as? String else {
            completionHandler(NSError(domain: "CallVPN", code: 1, userInfo: [NSLocalizedDescriptionKey: "Missing configuration"]))
            return
        }

        let serverAddr = (config["serverAddr"] as? String) ?? ""
        let numConns = (config["numConns"] as? Int) ?? 4
        let token = (config["token"] as? String) ?? ""

        let tunnelRemoteAddr = serverAddr.isEmpty ? "relay.vk.com" : serverAddr

        // Configure tunnel network settings
        let settings = NEPacketTunnelNetworkSettings(tunnelRemoteAddress: tunnelRemoteAddr)

        let ipv4 = NEIPv4Settings(addresses: ["10.0.0.2"], subnetMasks: ["255.255.255.0"])
        ipv4.includedRoutes = [NEIPv4Route.default()]
        settings.ipv4Settings = ipv4

        let dns = NEDNSSettings(servers: ["8.8.8.8", "8.8.4.4"])
        settings.dnsSettings = dns
        settings.mtu = 1280

        setTunnelNetworkSettings(settings) { [weak self] error in
            if let error = error {
                completionHandler(error)
                return
            }

            // Start Go tunnel
            let cfg = BindTunnelConfig()
            cfg.callLink = callLink
            cfg.serverAddr = serverAddr
            cfg.numConns = Int(numConns)
            cfg.useTCP = true
            cfg.token = token

            let t = BindNewTunnel()!

            var startError: NSError?
            t.start(cfg, error: &startError)

            if let err = startError {
                completionHandler(err)
                return
            }

            self?.tunnel = t
            self?.startLogForwarding()
            self?.startPacketForwarding()
            completionHandler(nil)
        }
    }

    private func startLogForwarding() {
        let timer = DispatchSource.makeTimerSource(queue: DispatchQueue.global(qos: .utility))
        timer.schedule(deadline: .now(), repeating: .milliseconds(500))
        timer.setEventHandler { [weak self] in
            guard let self = self, let tunnel = self.tunnel else { return }
            let logs = tunnel.readLogs()
            if !logs.isEmpty {
                tunnel.clearLogs()
                // Append to existing logs in UserDefaults
                let existing = self.logDefaults?.string(forKey: "vpn_logs") ?? ""
                let combined = existing.isEmpty ? logs : existing + "\n" + logs
                self.logDefaults?.set(combined, forKey: "vpn_logs")
            }
        }
        timer.resume()
        logTimer = timer
    }

    private func startPacketForwarding() {
        // Read packets from the TUN interface and send to tunnel
        packetFlow.readPackets { [weak self] packets, protocols in
            guard let self = self, let tunnel = self.tunnel else { return }

            for packet in packets {
                do {
                    try tunnel.writePacket(packet)
                } catch {
                    NSLog("CallVPN: write packet error: \(error)")
                }
            }

            // Continue reading
            self.startPacketForwarding()
        }

        // Read packets from tunnel and write to TUN interface
        DispatchQueue.global(qos: .userInteractive).async { [weak self] in
            guard let self = self, let tunnel = self.tunnel else { return }

            let buf = NSMutableData(length: 1500)!
            while tunnel.isRunning() {
                var len: Int = 0
                var error: NSError?
                len = tunnel.readPacket(buf.mutableBytes.assumingMemoryBound(to: UInt8.self), ret0_: buf.length, error: &error)

                if let _ = error { continue }
                if len > 0 {
                    let packet = Data(bytes: buf.bytes, count: len)
                    // Assume IPv4 (AF_INET = 2)
                    self.packetFlow.writePackets([packet], withProtocols: [NSNumber(value: AF_INET)])
                }
            }
        }
    }

    override func stopTunnel(with reason: NEProviderStopReason, completionHandler: @escaping () -> Void) {
        logTimer?.cancel()
        logTimer = nil
        logDefaults?.removeObject(forKey: "vpn_logs")
        tunnel?.stop()
        tunnel = nil
        completionHandler()
    }
}
