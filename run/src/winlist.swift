// winlist: prints the windows of Dolphin processes, one per line, tab separated:
//   windowID  pid  title  widthPoints  heightPoints  onScreen|offScreen
// A plain CGWindowListCopyWindowInfo query (no ScreenCaptureKit), so it returns at once.
// Used by nav/dolphin.py and run/stream_slot.sh instead of `wincap list`, which can hang.
// Build: swiftc -O run/src/winlist.swift -o run/bin/winlist   (run/lib.sh does it when missing)
import CoreGraphics
import Foundation
let l = (CGWindowListCopyWindowInfo([.optionAll], kCGNullWindowID) as? [[String: Any]]) ?? []
for w in l where (w["kCGWindowOwnerName"] as? String) == "Dolphin" {
  let b = (w["kCGWindowBounds"] as? [String: Any]) ?? [:]
  let on = ((w["kCGWindowIsOnscreen"] as? Int) ?? 0) != 0
  let title = ((w["kCGWindowName"] as? String) ?? "").replacingOccurrences(of: "\t", with: " ")
  print("\(w["kCGWindowNumber"] ?? 0)\t\(w["kCGWindowOwnerPID"] ?? 0)\t\(title)\t\(Int((b["Width"] as? Double) ?? 0))\t\(Int((b["Height"] as? Double) ?? 0))\t\(on ? "onScreen" : "offScreen")")
}
