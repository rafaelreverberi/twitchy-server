import Foundation
let data = try Data(contentsOf: URL(fileURLWithPath: CommandLine.arguments[1]))
let rows = try JSONSerialization.jsonObject(with: data) as! [[String: Any]]
var count = 0
for row in rows {
    guard let body = row["body"], !(body is NSNull) else { continue }
    let payload = try JSONSerialization.data(withJSONObject: body)
    let decoder = JSONCoding.decoder()
    if row["status"] as! Int >= 400 { _ = try decoder.decode(APIErrorEnvelope.self, from: payload) }
    else {
        switch row["route"] as! String {
        case "/v1/health": _ = try decoder.decode(Health.self, from: payload)
        case "/v1/capabilities": _ = try decoder.decode(Capabilities.self, from: payload)
        case "/v1/playback/sessions": _ = try decoder.decode(PlaybackSession.self, from: payload)
        default: continue
        }
    }
    count += 1
}
print("\(count) recorded responses decoded using the original Twitchy model definitions and JSONCoding")
