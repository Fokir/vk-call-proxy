import NetworkExtension
import Bind // gomobile generated framework

class PacketTunnelProvider: NEPacketTunnelProvider {

    private var tunnel: BindTunnel?

    override func startTunnel(options: [String: NSObject]?, completionHandler: @escaping (Error?) -> Void) {
        guard let callLink = options?["callLink"] as? String,
              let serverAddr = options?["serverAddr"] as? String else {
            completionHandler(NSError(domain: "CallVPN", code: 1, userInfo: [NSLocalizedDescriptionKey: "Missing configuration"]))
            return
        }

        let numConns = (options?["numConns"] as? Int) ?? 4
        let token = (options?["token"] as? String) ?? ""

        // Configure tunnel network settings
        let settings = NEPacketTunnelNetworkSettings(tunnelRemoteAddress: serverAddr)

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
            let config = BindTunnelConfig()
            config.callLink = callLink
            config.serverAddr = serverAddr
            config.numConns = Int(numConns)
            config.useTCP = true
            config.token = token

            let t = BindNewTunnel()!

            var startError: NSError?
            t.start(config, error: &startError)

            if let err = startError {
                completionHandler(err)
                return
            }

            self?.tunnel = t
            self?.startPacketForwarding()
            completionHandler(nil)
        }
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
        tunnel?.stop()
        tunnel = nil
        completionHandler()
    }
}
