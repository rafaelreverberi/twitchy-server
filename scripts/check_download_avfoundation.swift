// Verify completed download fixtures on macOS; no Twitch account or device needed.
import Foundation
import AVFoundation
import AudioToolbox
import CoreMedia

@main struct DownloadCheck {
    static func main() async {
        do { try await verify() }
        catch { print("AVFoundation verification failed: \(error)"); exit(1) }
    }
    static func verify() async throws {
        for path in CommandLine.arguments.dropFirst() {
            let asset = AVURLAsset(url: URL(fileURLWithPath: path))
            let video = try await asset.loadTracks(withMediaType: .video)
            let audio = try await asset.loadTracks(withMediaType: .audio)
            let expectsAudio = !path.contains("audio-false")
            let duration = try await asset.load(.duration).seconds
            print("\(URL(fileURLWithPath: path).lastPathComponent): video=\(video.count) audio=\(audio.count) duration=\(duration)")
            guard video.count == 1, audio.count == (expectsAudio ? 1 : 0), duration >= 5.9, duration < 6.3 else {
                throw NSError(domain: "DownloadCheck", code: 1, userInfo: [NSLocalizedDescriptionKey: "Missing tracks or incomplete duration"])
            }
            guard expectsAudio else { continue }
            let reader = try AVAssetReader(asset: asset)
            let output = AVAssetReaderTrackOutput(track: audio[0], outputSettings: [
                AVFormatIDKey: kAudioFormatLinearPCM,
                AVLinearPCMBitDepthKey: 32,
                AVLinearPCMIsFloatKey: true,
                AVLinearPCMIsBigEndianKey: false,
                AVLinearPCMIsNonInterleaved: false
            ])
            reader.add(output)
            guard reader.startReading() else { throw reader.error! }
            var energy = [Double](repeating: 0, count: 7)
            var counts = [Int](repeating: 0, count: 7)
            var frames = 0
            var end = 0.0
            while let sample = output.copyNextSampleBuffer() {
                let pts = CMSampleBufferGetPresentationTimeStamp(sample).seconds
                let sampleFrames = CMSampleBufferGetNumSamples(sample)
                frames += sampleFrames
                end = max(end, pts + Double(sampleFrames) / 48000)
                guard let block = CMSampleBufferGetDataBuffer(sample) else { fatalError("Missing decoded PCM") }
                let byteCount = CMBlockBufferGetDataLength(block)
                var values = [Float](repeating: 0, count: byteCount / 4)
                let status = values.withUnsafeMutableBytes {
                    CMBlockBufferCopyDataBytes(block, atOffset: 0, dataLength: byteCount, destination: $0.baseAddress!)
                }
                guard status == kCMBlockBufferNoErr, values.count == sampleFrames * 2 else { fatalError("Invalid stereo PCM") }
                for (index, value) in values.enumerated() {
                    let second = min(6, max(0, Int(pts + Double(index / 2) / 48000)))
                    energy[second] += Double(value) * Double(value)
                    counts[second] += 1
                }
            }
            guard reader.status == .completed, frames >= 283200, frames <= 297600, end >= 5.9 else {
                throw NSError(domain: "DownloadCheck", code: 2, userInfo: [NSLocalizedDescriptionKey: "Incomplete audio decode: \(String(describing: reader.error))"])
            }
            for second in 0..<6 {
                let rms = sqrt(energy[second] / Double(max(1, counts[second])))
                guard counts[second] > 0, rms > 0.02, rms < 0.2 else { fatalError("No test tone at second \(second)") }
            }
            print("AVFoundation decoded \(frames) stereo frames through \(end)s; tone energy present in all six seconds")
        }
    }
}
