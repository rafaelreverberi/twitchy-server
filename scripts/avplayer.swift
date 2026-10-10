// macOS verification tool; does not modify Twitchy sources or read its Keychain.
import Foundation
import AVFoundation
@main struct AVPlayerSmoke {
    @MainActor static func main() async throws {
        let base = CommandLine.arguments[1].trimmingCharacters(in: CharacterSet(charactersIn: "/"))
        let channel = CommandLine.arguments[2]
        let tokens = try JSONSerialization.jsonObject(with: Data(contentsOf: URL(fileURLWithPath: "secrets/tokens.json"))) as! [String:String]
        let token = tokens.values.first!
        var request = URLRequest(url: URL(string: base + "/v1/playback/sessions")!)
        request.httpMethod = "POST"
        request.setValue("Bearer " + token, forHTTPHeaderField: "Authorization")
        request.setValue("application/json", forHTTPHeaderField: "Content-Type")
        request.httpBody = try JSONSerialization.data(withJSONObject: ["content_type":"live", "channel_login":channel, "requested_quality":"360p30"])
        request.timeoutInterval = 100
        let (data,response) = try await URLSession.shared.data(for: request)
        guard (response as? HTTPURLResponse)?.statusCode == 201,
              let session = try JSONSerialization.jsonObject(with: data) as? [String:Any],
              let id = session["session_id"] as? String,
              let text = session["hls_url"] as? String, let url = URL(string:text) else {
            print("AVPlayer smoke FAILED: session acquisition"); exit(1)
        }
        let player = AVPlayer(url:url);player.isMuted=true;player.play()
        for _ in 0..<25 {
            try await Task.sleep(for:.seconds(1))
            if player.currentTime().seconds >= 5 || player.status == .failed {break}
        }
        let seconds = player.currentTime().seconds
        let success = player.status == .readyToPlay && seconds >= 5
        player.pause();player.replaceCurrentItem(with:nil)
        var remove = URLRequest(url:URL(string:base+"/v1/playback/sessions/"+id)!)
        remove.httpMethod="DELETE";remove.setValue("Bearer "+token,forHTTPHeaderField:"Authorization")
        let (_,deleted) = try await URLSession.shared.data(for:remove)
        guard success && (deleted as? HTTPURLResponse)?.statusCode==204 else {
            print("AVPlayer smoke FAILED: status/timeline; underlying URLs withheld");exit(1)
        }
        print("macOS AVPlayer readyToPlay; media clock advanced \(Int(seconds)) seconds; DELETE passed")
    }
}
