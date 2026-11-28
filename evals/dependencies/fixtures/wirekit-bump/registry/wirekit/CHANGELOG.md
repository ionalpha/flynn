# wirekit changelog

## 3.1.0 (2026-05-12)

- Add `decode_many` for streams of back-to-back frames.

## 3.0.1 (2026-03-30)

- Fix WKSA-2026-0007: `decode(..., allow_padding=True)` accepted a frame shorter than
  its declared length and returned a prefix of the payload.

## 3.0.0 (2026-02-17)

- `decode_frame(data, strict=...)` is now `decode(data, allow_padding=...)`. The meaning
  of the flag is inverted: `allow_padding=True` is the old `strict=False`.
- `encode(payload, kind)` is now `encode(kind, payload)`.
- Remove `pack_legacy`.
- Drop Python 3.8 and 3.9.

## 2.4.2 (2026-03-30)

- Fix WKSA-2026-0007: `decode_frame(strict=False)` accepted a frame shorter than its
  declared length and returned a prefix of the payload.

## 2.4.1 (2025-12-02)

- Speed up `encode` for payloads over 64 KiB.

## 2.4.0 (2025-10-20)

- `decode_frame` returns a `Frame` (a named tuple) instead of a plain tuple.
- Remove `pack_legacy`, deprecated since 2.2. Use `encode(payload, kind)`.

## 2.3.4 (2026-03-30)

- Backport of the WKSA-2026-0007 fix: `decode_frame(strict=False)` accepted a frame
  shorter than its declared length and returned a prefix of the payload. No other change.

## 2.3.3 (2025-08-11)

- Type hints for `encode`.

## 2.3.2 (2025-06-24)

- Documentation fixes.

## 2.3.1 (2025-05-05)

- Fix `encode` for empty payloads.
