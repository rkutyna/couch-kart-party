// ocr: reads text in images with Apple's Vision framework (VNRecognizeTextRequest), fully
// offline. Used only by the navigator's calibration (nav/calibrate.py, nav/ocrscreen.py) to
// tell the game's screens apart before any screen template exists.
//
// Server mode (default): one request per line on stdin, one JSON line per request on stdout.
//   request:  <image path>[\t<x0> <y0> <x1> <y1>]      (optional crop, pixels, top-left origin)
//   answer:   [{"t": text, "c": confidence, "b": [x0, y0, x1, y1]}, ...]   (pixels of the
//             full image, top-left origin) or {"error": "..."}
// One-shot:  ocr <image> [x0 y0 x1 y1]
// Build: swiftc -O run/src/ocr.swift -o run/bin/ocr   (./mkparty setup does it)
import CoreGraphics
import Foundation
import ImageIO
import Vision

func loadImage(_ path: String) -> CGImage? {
  guard let src = CGImageSourceCreateWithURL(URL(fileURLWithPath: path) as CFURL, nil) else { return nil }
  return CGImageSourceCreateImageAtIndex(src, 0, nil)
}

func esc(_ s: String) -> String {
  var o = ""
  for ch in s.unicodeScalars {
    switch ch {
    case "\"": o += "\\\""
    case "\\": o += "\\\\"
    case "\n": o += " "
    case "\t": o += " "
    default:
      if ch.value < 0x20 { o += " " } else { o.unicodeScalars.append(ch) }
    }
  }
  return o
}

func recognize(_ path: String, _ crop: [Int]?) -> String {
  guard var img = loadImage(path) else { return "{\"error\": \"cannot read \(esc(path))\"}" }
  var ox = 0, oy = 0
  if let c = crop, c.count == 4 {
    let r = CGRect(x: c[0], y: c[1], width: max(1, c[2] - c[0]), height: max(1, c[3] - c[1]))
    guard let sub = img.cropping(to: r) else { return "{\"error\": \"bad crop\"}" }
    img = sub
    ox = c[0]; oy = c[1]
  }
  let W = Double(img.width), H = Double(img.height)
  let req = VNRecognizeTextRequest()
  req.recognitionLevel = .accurate
  req.usesLanguageCorrection = false
  req.recognitionLanguages = ["en-US"]
  req.minimumTextHeight = 0.0
  let h = VNImageRequestHandler(cgImage: img, options: [:])
  do { try h.perform([req]) } catch { return "{\"error\": \"\(esc(String(describing: error)))\"}" }
  var parts: [String] = []
  for o in req.results ?? [] {
    guard let cand = o.topCandidates(1).first else { continue }
    let bb = o.boundingBox   // normalised, bottom-left origin
    let x0 = Int(bb.minX * W) + ox, x1 = Int(bb.maxX * W) + ox
    let y0 = Int((1 - bb.maxY) * H) + oy, y1 = Int((1 - bb.minY) * H) + oy
    parts.append("{\"t\": \"\(esc(cand.string))\", \"c\": \(String(format: "%.2f", cand.confidence)), \"b\": [\(x0), \(y0), \(x1), \(y1)]}")
  }
  return "[" + parts.joined(separator: ", ") + "]"
}

setvbuf(stdout, nil, _IOLBF, 0)
let args = CommandLine.arguments
if args.count >= 2 {
  let crop = args.count >= 6 ? args[2...5].compactMap { Int($0) } : nil
  print(recognize(args[1], crop))
  exit(0)
}
while let line = readLine() {
  let f = line.split(separator: "\t", maxSplits: 1).map(String.init)
  if f.isEmpty { continue }
  var crop: [Int]? = nil
  if f.count > 1 { crop = f[1].split(separator: " ").compactMap { Int($0) } }
  print(recognize(f[0], crop))
}
