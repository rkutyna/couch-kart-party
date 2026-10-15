// wincap: capture ONE window with ScreenCaptureKit and write raw NV12 (420v)
// frames to stdout at a constant frame rate.
//
//   wincap list
//   wincap capture (--window <id> | --pid <pid> [--title-contains <text>])
//                  [--fps 60] [--width W --height H] [--crop x,y,w,h]
//                  [--exclude-titlebar [--titlebar-height 28]]
//                  [--stats] [--print-size-only] [--duration <seconds>]
//
// Output frame layout: Y plane (W*H bytes) followed by interleaved UV plane
// (W*H/2 bytes), no row padding. W and H are always even.
// Build:
//   swiftc -O -swift-version 5 -o capture/bin/wincap capture/wincap.swift

import Foundation
import ScreenCaptureKit
import CoreMedia
import CoreVideo
import CoreGraphics
import Darwin

// MARK: - Helpers

func eprint(_ s: String) {
    if let d = (s + "\n").data(using: .utf8) {
        FileHandle.standardError.write(d)
    }
}

func fail(_ s: String, code: Int32 = 1) -> Never {
    eprint("wincap: error: \(s)")
    exit(code)
}

func usage() -> Never {
    eprint("""
    usage:
      wincap list
      wincap capture (--window <id> | --pid <pid> [--title-contains <text>])
                     [--fps 60] [--width W --height H] [--crop x,y,w,h]
                     [--exclude-titlebar [--titlebar-height 28]] [--stats]
                     [--print-size-only] [--duration <seconds>]
    """)
    exit(2)
}

// MARK: - Options

struct Options {
    var command = ""
    var windowID: CGWindowID? = nil
    var pid: pid_t? = nil
    var titleContains: String? = nil
    var fps = 60
    var width: Int? = nil
    var height: Int? = nil
    var crop: CGRect? = nil          // points, relative to window top-left
    var excludeTitlebar = false
    var titlebarHeight: CGFloat = 28 // points
    var stats = false
    var printSizeOnly = false
    var duration: Double? = nil
}

func parseOptions() -> Options {
    var o = Options()
    let args = Array(CommandLine.arguments.dropFirst())
    guard let cmd = args.first else { usage() }
    o.command = cmd
    var i = 1

    func next(_ flag: String) -> String {
        i += 1
        guard i < args.count else { fail("\(flag) requires a value") }
        return args[i]
    }
    func positiveInt(_ flag: String) -> Int {
        guard let v = Int(next(flag)), v > 0 else { fail("\(flag) needs a positive integer") }
        return v
    }

    while i < args.count {
        switch args[i] {
        case "--window":
            guard let v = UInt32(next("--window")) else { fail("--window needs an integer id") }
            o.windowID = CGWindowID(v)
        case "--pid":
            guard let v = Int32(next("--pid")) else { fail("--pid needs an integer pid") }
            o.pid = pid_t(v)
        case "--title-contains":
            o.titleContains = next("--title-contains")
        case "--fps":
            o.fps = positiveInt("--fps")
        case "--width":
            o.width = positiveInt("--width")
        case "--height":
            o.height = positiveInt("--height")
        case "--crop":
            let parts = next("--crop").split(separator: ",").compactMap { Double($0) }
            guard parts.count == 4, parts[2] > 0, parts[3] > 0 else {
                fail("--crop expects x,y,w,h in points")
            }
            o.crop = CGRect(x: parts[0], y: parts[1], width: parts[2], height: parts[3])
        case "--exclude-titlebar":
            o.excludeTitlebar = true
        case "--titlebar-height":
            guard let v = Double(next("--titlebar-height")), v >= 0 else {
                fail("--titlebar-height needs a number >= 0")
            }
            o.titlebarHeight = CGFloat(v)
        case "--stats":
            o.stats = true
        case "--print-size-only":
            o.printSizeOnly = true
        case "--duration":
            guard let v = Double(next("--duration")), v > 0 else { fail("--duration needs seconds > 0") }
            o.duration = v
        case "-h", "--help":
            usage()
        default:
            fail("unknown argument \(args[i])")
        }
        i += 1
    }
    return o
}

// MARK: - list

func cmdList() async throws {
    let content = try await SCShareableContent.excludingDesktopWindows(false, onScreenWindowsOnly: false)
    for w in content.windows.sorted(by: { $0.windowID < $1.windowID }) {
        let pid = w.owningApplication?.processID ?? -1
        let app = w.owningApplication?.applicationName ?? "?"
        let title = (w.title ?? "").replacingOccurrences(of: "\t", with: " ")
        print("\(w.windowID)\t\(pid)\t\(app)\t\(title)\t\(Int(w.frame.width))\t\(Int(w.frame.height))\t\(w.isOnScreen ? "onScreen" : "offScreen")\tlayer=\(w.windowLayer)")
    }
    fflush(stdout)
}

// MARK: - capture geometry

func selectWindow(_ content: SCShareableContent, _ o: Options) -> SCWindow {
    if let id = o.windowID {
        guard let w = content.windows.first(where: { $0.windowID == id }) else {
            fail("no capturable window with id \(id) (run `wincap list`)")
        }
        return w
    }
    guard let pid = o.pid else { fail("need --window <id> or --pid <pid>") }
    var cands = content.windows.filter { $0.owningApplication?.processID == pid }
    if let t = o.titleContains {
        cands = cands.filter { ($0.title ?? "").localizedCaseInsensitiveContains(t) }
    }
    guard !cands.isEmpty else {
        fail("no capturable window for pid \(pid)\(o.titleContains.map { " with title containing \"\($0)\"" } ?? "")")
    }
    cands.sort { a, b in
        if a.isOnScreen != b.isOnScreen { return a.isOnScreen }
        return a.frame.width * a.frame.height > b.frame.width * b.frame.height
    }
    if cands.count > 1 {
        eprint("wincap: note: \(cands.count) windows match pid \(pid); using id \(cands[0].windowID) \"\(cands[0].title ?? "")\" (largest on-screen)")
    }
    return cands[0]
}

struct Geometry {
    let filter: SCContentFilter
    let sourceRect: CGRect?   // points, relative to window top-left; nil = whole window
    let width: Int            // output pixels (even)
    let height: Int           // output pixels (even)
}

func geometry(for window: SCWindow, _ o: Options) -> Geometry {
    let filter = SCContentFilter(desktopIndependentWindow: window)
    let full = filter.contentRect.size            // points
    let scale = CGFloat(filter.pointPixelScale)   // pixels per point
    guard full.width > 0, full.height > 0, scale > 0 else {
        fail("window \(window.windowID) reports an empty content rect")
    }
    let fullRect = CGRect(origin: .zero, size: full)

    var rect = fullRect
    if o.excludeTitlebar {
        rect = CGRect(x: 0, y: o.titlebarHeight, width: full.width,
                      height: full.height - o.titlebarHeight)
    }
    if let c = o.crop {
        rect = o.excludeTitlebar ? c.intersection(rect) : c
    }
    guard rect.width >= 2, rect.height >= 2, fullRect.insetBy(dx: -0.01, dy: -0.01).contains(rect) else {
        fail("crop/titlebar rect \(rect) is empty or outside the window \(fullRect)")
    }

    let naturalW = max(2, Int((rect.width * scale).rounded()))
    let naturalH = max(2, Int((rect.height * scale).rounded()))
    var ow = o.width ?? naturalW
    var oh = o.height ?? naturalH
    if o.width != nil && o.height == nil { oh = Int((Double(naturalH) * Double(ow) / Double(naturalW)).rounded()) }
    if o.height != nil && o.width == nil { ow = Int((Double(naturalW) * Double(oh) / Double(naturalH)).rounded()) }
    ow &= ~1   // NV12 needs even dimensions
    oh &= ~1
    guard ow >= 2, oh >= 2 else { fail("output size \(ow)x\(oh) too small") }

    let useSource = rect != fullRect
    return Geometry(filter: filter, sourceRect: useSource ? rect : nil, width: ow, height: oh)
}

// MARK: - Capturer

final class Capturer: NSObject, SCStreamOutput, SCStreamDelegate {
    let windowID: CGWindowID
    let fps: Int
    let width: Int
    let height: Int
    let frameBytes: Int
    let showStats: Bool

    let callbackQueue = DispatchQueue(label: "wincap.sck", qos: .userInitiated)
    private let writerQueue = DispatchQueue(label: "wincap.writer", qos: .userInitiated)
    private let lock = NSLock()

    // Two packed NV12 buffers. The SCK callback fills `bufPending` and sets
    // `hasPending`; the writer tick swaps it into `bufCurrent` and writes it.
    // Pointers are swapped only on the writer queue, under `lock`.
    private var bufPending: UnsafeMutablePointer<UInt8>
    private var bufCurrent: UnsafeMutablePointer<UInt8>
    private var hasPending = false
    private var haveFrame = false
    private var shuttingDown = false

    // Counters, guarded by `lock`.
    private var cCaptured = 0
    private var cIdle = 0
    private var cSizeMismatch = 0
    private var cNew = 0
    private var cRepeat = 0
    private var cTicks = 0

    private var timer: DispatchSourceTimer?
    var stream: SCStream?

    init(windowID: CGWindowID, fps: Int, width: Int, height: Int, showStats: Bool) {
        self.windowID = windowID
        self.fps = fps
        self.width = width
        self.height = height
        self.frameBytes = width * height * 3 / 2
        self.showStats = showStats
        bufPending = UnsafeMutablePointer<UInt8>.allocate(capacity: width * height * 3 / 2)
        bufCurrent = UnsafeMutablePointer<UInt8>.allocate(capacity: width * height * 3 / 2)
        super.init()
    }

    func startTimer() {
        let t = DispatchSource.makeTimerSource(queue: writerQueue)
        t.schedule(deadline: .now(), repeating: .nanoseconds(1_000_000_000 / fps), leeway: .microseconds(50))
        t.setEventHandler { [weak self] in self?.tick() }
        t.resume()
        timer = t
    }

    func stopTimer() {
        lock.lock()
        shuttingDown = true
        lock.unlock()
        timer?.cancel()
        timer = nil
    }

    // Called on writerQueue once per output frame slot.
    private func tick() {
        lock.lock()
        var fresh = false
        if hasPending {
            let t = bufPending
            bufPending = bufCurrent
            bufCurrent = t
            hasPending = false
            haveFrame = true
            fresh = true
        }
        if fresh { cNew += 1 } else if haveFrame { cRepeat += 1 }
        cTicks += 1
        var statLine: String? = nil
        if showStats && cTicks >= fps {
            statLine = "STATS captured=\(cCaptured) idle=\(cIdle) sizeMismatch=\(cSizeMismatch) new=\(cNew) repeated=\(cRepeat) emitted=\(cNew + cRepeat)"
            cCaptured = 0; cIdle = 0; cSizeMismatch = 0
            cNew = 0; cRepeat = 0; cTicks = 0
        }
        let out: UnsafeMutablePointer<UInt8>? = haveFrame ? bufCurrent : nil
        lock.unlock()

        if let s = statLine { eprint(s) }
        // bufCurrent is only swapped on this queue, so writing it unlocked is safe;
        // the SCK callback only touches bufPending.
        if let out = out {
            writeAll(out, frameBytes)
        }
    }

    private func writeAll(_ p: UnsafeMutablePointer<UInt8>, _ n: Int) {
        var off = 0
        while off < n {
            let r = write(1, p + off, n - off)
            if r < 0 {
                if errno == EINTR { continue }
                if errno == EPIPE { exit(0) }   // consumer went away
                fail("write to stdout failed: \(String(cString: strerror(errno)))")
            }
            off += r
        }
    }

    // Copy one complete NV12 pixel buffer into bufPending, honouring bytesPerRow.
    private func store(_ pb: CVPixelBuffer) {
        CVPixelBufferLockBaseAddress(pb, .readOnly)
        defer { CVPixelBufferUnlockBaseAddress(pb, .readOnly) }

        guard CVPixelBufferGetPlaneCount(pb) == 2,
              CVPixelBufferGetWidth(pb) == width,
              CVPixelBufferGetHeight(pb) == height,
              let yBase = CVPixelBufferGetBaseAddressOfPlane(pb, 0),
              let uvBase = CVPixelBufferGetBaseAddressOfPlane(pb, 1) else {
            lock.lock(); cSizeMismatch += 1; lock.unlock()
            return
        }
        let yStride = CVPixelBufferGetBytesPerRowOfPlane(pb, 0)
        let uvStride = CVPixelBufferGetBytesPerRowOfPlane(pb, 1)
        let ySrc = yBase.assumingMemoryBound(to: UInt8.self)
        let uvSrc = uvBase.assumingMemoryBound(to: UInt8.self)

        lock.lock()
        defer { lock.unlock() }
        let dst = bufPending
        for row in 0..<height {
            memcpy(dst + row * width, ySrc + row * yStride, width)
        }
        let uvDst = dst + width * height
        for row in 0..<(height / 2) {
            memcpy(uvDst + row * width, uvSrc + row * uvStride, width)
        }
        hasPending = true
        cCaptured += 1
    }

    // MARK: SCStreamOutput

    func stream(_ stream: SCStream, didOutputSampleBuffer sampleBuffer: CMSampleBuffer, of type: SCStreamOutputType) {
        guard type == .screen, sampleBuffer.isValid else { return }
        guard let arr = CMSampleBufferGetSampleAttachmentsArray(sampleBuffer, createIfNecessary: false) as? [[SCStreamFrameInfo: Any]],
              let info = arr.first,
              let raw = info[.status] as? Int,
              let status = SCFrameStatus(rawValue: raw) else { return }

        switch status {
        case .complete:
            guard let pb = CMSampleBufferGetImageBuffer(sampleBuffer) else { return }
            store(pb)
        case .idle:
            // No content change since last frame: the timer repeats the last frame.
            lock.lock(); cIdle += 1; lock.unlock()
        case .stopped:
            lock.lock(); let s = shuttingDown; lock.unlock()
            if !s { fail("stream stopped by the system (window closed or capture revoked?)") }
        default:
            break   // started / blank / suspended: no image
        }
    }

    // MARK: SCStreamDelegate

    func stream(_ stream: SCStream, didStopWithError error: Error) {
        lock.lock(); let s = shuttingDown; lock.unlock()
        if !s { fail("stream stopped with error: \(error.localizedDescription)") }
    }
}

// MARK: - capture command

var activeCapturer: Capturer? = nil

func cmdCapture(_ o: Options) async {
    if !o.printSizeOnly && isatty(1) != 0 {
        fail("refusing to write raw video to a terminal; pipe stdout into ffmpeg")
    }
    if o.windowID == nil && o.pid == nil { fail("need --window <id> or --pid <pid>") }
    if o.windowID != nil && o.pid != nil { fail("use either --window or --pid, not both") }
    if o.titleContains != nil && o.pid == nil { fail("--title-contains needs --pid") }

    let content: SCShareableContent
    do {
        content = try await SCShareableContent.excludingDesktopWindows(false, onScreenWindowsOnly: false)
    } catch {
        fail("SCShareableContent failed (Screen Recording permission?): \(error.localizedDescription)")
    }

    let win = selectWindow(content, o)
    let geo = geometry(for: win, o)
    let sizeLine = "SIZE \(geo.width)x\(geo.height) FPS \(o.fps)"

    if o.printSizeOnly {
        print("\(sizeLine) WINDOW \(win.windowID)")
        fflush(stdout)
        exit(0)
    }

    let cap = Capturer(windowID: win.windowID, fps: o.fps, width: geo.width,
                       height: geo.height, showStats: o.stats)
    activeCapturer = cap

    let cfg = SCStreamConfiguration()
    cfg.width = geo.width
    cfg.height = geo.height
    if let r = geo.sourceRect { cfg.sourceRect = r }
    cfg.minimumFrameInterval = CMTime(value: 1, timescale: CMTimeScale(o.fps))
    cfg.queueDepth = 3
    cfg.pixelFormat = kCVPixelFormatType_420YpCbCr8BiPlanarVideoRange   // '420v' = NV12
    cfg.showsCursor = false
    cfg.capturesAudio = false
    cfg.ignoreShadowsSingleWindow = true
    cfg.ignoreGlobalClipSingleWindow = true

    let stream = SCStream(filter: geo.filter, configuration: cfg, delegate: cap)
    cap.stream = stream
    do {
        try stream.addStreamOutput(cap, type: .screen, sampleHandlerQueue: cap.callbackQueue)
    } catch {
        fail("addStreamOutput failed: \(error.localizedDescription)")
    }

    // SIZE line goes out before the stream starts, so before any frame.
    eprint(sizeLine)

    do {
        try await stream.startCapture()
    } catch {
        fail("startCapture failed: \(error.localizedDescription)")
    }
    cap.startTimer()

    // Watchdog: exit with an error if the window disappears.
    let wid = win.windowID
    Task.detached {
        while true {
            try? await Task.sleep(nanoseconds: 1_000_000_000)
            if let c = try? await SCShareableContent.excludingDesktopWindows(false, onScreenWindowsOnly: false) {
                if !c.windows.contains(where: { $0.windowID == wid }) {
                    fail("window \(wid) disappeared")
                }
            }
        }
    }

    if let d = o.duration {
        DispatchQueue.main.asyncAfter(deadline: .now() + d) { shutdown(code: 0) }
    }
}

func shutdown(code: Int32) {
    activeCapturer?.stopTimer()
    let s = activeCapturer?.stream
    DispatchQueue.global().asyncAfter(deadline: .now() + 2) { exit(code) }   // safety net
    Task {
        try? await s?.stopCapture()
        exit(code)
    }
}

// MARK: - main

let opts = parseOptions()

// Fix 1 for CGS_REQUIRE_INIT assertion (SCContentFilter used before the
// WindowServer connection exists in a non-AppKit process): touch CoreGraphics first.
_ = CGMainDisplayID()

signal(SIGPIPE, SIG_IGN)   // broken pipe shows up as EPIPE from write()
signal(SIGINT, SIG_IGN)
signal(SIGTERM, SIG_IGN)
let sigInt = DispatchSource.makeSignalSource(signal: SIGINT, queue: .main)
sigInt.setEventHandler { shutdown(code: 0) }
sigInt.resume()
let sigTerm = DispatchSource.makeSignalSource(signal: SIGTERM, queue: .main)
sigTerm.setEventHandler { shutdown(code: 0) }
sigTerm.resume()

switch opts.command {
case "list":
    Task {
        do {
            try await cmdList()
            exit(0)
        } catch {
            fail("SCShareableContent failed (Screen Recording permission?): \(error.localizedDescription)")
        }
    }
    dispatchMain()
case "capture":
    Task { await cmdCapture(opts) }
    dispatchMain()
default:
    usage()
}
