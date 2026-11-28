# WKSA-2026-0007: wirekit accepts truncated frames

Severity: high.

`wirekit.decode_frame(data, strict=False)` (`wirekit.decode(data, allow_padding=True)` in
3.x) returns a prefix of the payload when the frame is shorter than its declared length,
without checking the checksum. A truncated frame is therefore accepted as a valid, shorter
message. Strict decoding is not affected.

Affected: every release before the fix on its line.

Fixed in: 3.0.1.

Credit: reported by the maintainers of a downstream gateway.
