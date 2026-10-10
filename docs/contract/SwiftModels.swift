import Foundation

public struct Capabilities: Codable, Sendable, Equatable {
    public var apiVersion: String
    public var playbackProtocols: [String]
    public var live: Bool
    public var vod: Bool
    public var clips: Bool
    public var qualitySelection: Bool
    public var downloads: Bool
    public var authorizedDownloadsOnly: Bool
    public var adHandling: String?
    public init(apiVersion: String = "v1", playbackProtocols: [String] = ["hls"], live: Bool = true, vod: Bool = false, clips: Bool = false, qualitySelection: Bool = false, downloads: Bool = false, authorizedDownloadsOnly: Bool = true, adHandling: String? = nil) { self.apiVersion = apiVersion; self.playbackProtocols = playbackProtocols; self.live = live; self.vod = vod; self.clips = clips; self.qualitySelection = qualitySelection; self.downloads = downloads; self.authorizedDownloadsOnly = authorizedDownloadsOnly; self.adHandling = adHandling }
}
public struct Health: Codable, Sendable { public var status: String; public var serverVersion: String; public var apiVersion: String }
public struct StreamingSource: Codable, Identifiable, Sendable, Equatable {
    public var id: UUID; public var name: String; public var baseURL: URL; public var enabled: Bool; public var allowLocalHTTP: Bool
    public var usesAuthentication: Bool; public var serverVersion: String?; public var capabilities: Capabilities?; public var lastSuccessfulCheck: Date?; public var connectionStatus: String
    public init(id: UUID = UUID(), name: String, baseURL: URL, allowLocalHTTP: Bool = false, usesAuthentication: Bool = false) { self.id = id; self.name = name; self.baseURL = baseURL; enabled = true; self.allowLocalHTTP = allowLocalHTTP; self.usesAuthentication = usesAuthentication; connectionStatus = "Not tested" }
}
public struct PlaybackRequest: Codable, Sendable {
    public var contentType: String; public var channelLogin: String?; public var contentId: String?; public var requestedQuality: String; public var preferences: Preferences?
    public init(contentType: String = "live", channelLogin: String? = nil, contentId: String? = nil, requestedQuality: String = "auto", preferences: Preferences? = nil) { self.contentType = contentType; self.channelLogin = channelLogin; self.contentId = contentId; self.requestedQuality = requestedQuality; self.preferences = preferences }
    public struct Preferences: Codable, Sendable { public var lowLatency: Bool; public init(lowLatency: Bool = true) { self.lowLatency = lowLatency } }
}
public struct PlaybackSession: Codable, Sendable {
    public var sessionId: String; public var hlsUrl: URL; public var qualities: [String]; public var expiresAt: Date?; public var title: String?; public var playerCapabilities: [String]?; public var filteringStatus: String?; public var state: String?
}
public enum DownloadState: String, Codable, Sendable { case queued, preparing, ready, transferring, paused, completed, failed, cancelled }
public struct DownloadJob: Codable, Identifiable, Sendable {
    public var id: String; public var status: DownloadState; public var progress: Double; public var transferredBytes: Int64; public var totalBytes: Int64?; public var error: APIProblem?; public var fileUrl: URL?; public var filename: String?; public var controls: [String]?
}
public struct APIProblem: Codable, Sendable { public var code: String; public var message: String; public var retryable: Bool }
public struct APIErrorEnvelope: Codable, Sendable { public var error: APIProblem }

public enum JSONCoding {
    public static func decoder() -> JSONDecoder { let d = JSONDecoder(); d.keyDecodingStrategy = .convertFromSnakeCase; d.dateDecodingStrategy = .iso8601; return d }
    public static func encoder() -> JSONEncoder { let e = JSONEncoder(); e.keyEncodingStrategy = .convertToSnakeCase; e.dateEncodingStrategy = .iso8601; return e }
}
