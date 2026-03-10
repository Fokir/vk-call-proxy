import Foundation

struct Profile: Codable, Identifiable {
    var id: UUID
    var name: String
    var connectionMode: String // "relay" or "direct"
    var callLink: String
    var serverAddr: String
    var token: String
    var numConns: Int

    init(
        id: UUID = UUID(),
        name: String = "",
        connectionMode: String = "relay",
        callLink: String = "",
        serverAddr: String = "",
        token: String = "",
        numConns: Int = 4
    ) {
        self.id = id
        self.name = name
        self.connectionMode = connectionMode
        self.callLink = callLink
        self.serverAddr = serverAddr
        self.token = token
        self.numConns = numConns
    }

    var isTelemostLink: Bool {
        callLink.contains("telemost.yandex") ||
        (callLink.allSatisfy(\.isNumber) && callLink.count > 10)
    }
}

class ProfileManager: ObservableObject {
    @Published var profiles: [Profile] = []
    @Published var activeProfileId: UUID?

    private let defaults = UserDefaults.standard
    private let profilesKey = "profiles_data"
    private let activeIdKey = "active_profile_id"

    init() {
        loadProfiles()
        migrateFromLegacy()
    }

    var activeProfile: Profile? {
        guard let id = activeProfileId else { return nil }
        return profiles.first { $0.id == id }
    }

    func setActiveProfile(_ id: UUID?) {
        activeProfileId = id
        if let id = id {
            defaults.set(id.uuidString, forKey: activeIdKey)
        } else {
            defaults.removeObject(forKey: activeIdKey)
        }
    }

    func saveProfile(_ profile: Profile) {
        if let idx = profiles.firstIndex(where: { $0.id == profile.id }) {
            profiles[idx] = profile
        } else {
            profiles.append(profile)
        }
        persistProfiles()
    }

    func deleteProfile(_ id: UUID) {
        guard let idx = profiles.firstIndex(where: { $0.id == id }) else { return }
        profiles.remove(at: idx)
        persistProfiles()

        if activeProfileId == id {
            let nextActive: UUID? = profiles.isEmpty ? nil :
                profiles[min(idx, profiles.count - 1)].id
            setActiveProfile(nextActive)
        }
    }

    private func loadProfiles() {
        if let data = defaults.data(forKey: profilesKey),
           let decoded = try? JSONDecoder().decode([Profile].self, from: data) {
            profiles = decoded
        }
        if let idStr = defaults.string(forKey: activeIdKey) {
            activeProfileId = UUID(uuidString: idStr)
        }
    }

    private func persistProfiles() {
        if let data = try? JSONEncoder().encode(profiles) {
            defaults.set(data, forKey: profilesKey)
        }
    }

    private func migrateFromLegacy() {
        // Already migrated
        if defaults.data(forKey: profilesKey) != nil { return }

        let callLink = defaults.string(forKey: "callLink") ?? ""
        guard !callLink.isEmpty else { return }

        let serverAddr = defaults.string(forKey: "serverAddr") ?? ""
        let token = defaults.string(forKey: "token") ?? ""
        let modeRaw = defaults.string(forKey: "connectionMode") ?? "relay"
        let numConnsStr = defaults.string(forKey: "numConns") ?? "4"
        let numConns = Int(numConnsStr) ?? 4

        let profile = Profile(
            name: "Default",
            connectionMode: modeRaw,
            callLink: callLink,
            serverAddr: serverAddr,
            token: token,
            numConns: numConns
        )

        profiles = [profile]
        setActiveProfile(profile.id)
        persistProfiles()

        // Clean up legacy keys
        defaults.removeObject(forKey: "callLink")
        defaults.removeObject(forKey: "serverAddr")
        defaults.removeObject(forKey: "token")
        defaults.removeObject(forKey: "connectionMode")
        defaults.removeObject(forKey: "numConns")
        defaults.removeObject(forKey: "recentIds")
    }
}
