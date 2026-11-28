"""wirekit 2.3.4: length-prefixed frames."""

import struct

__version__ = "2.3.4"
MAGIC = b"WK"


def _sum(payload):
    return bytes([sum(payload) % 256])


def encode(payload, kind):
    return MAGIC + bytes([kind]) + struct.pack(">H", len(payload)) + payload + _sum(payload)


def pack_legacy(kind, payload):
    return encode(payload, kind)


def decode_frame(data, strict=True):
    if data[:2] != MAGIC:
        raise ValueError("bad magic")
    kind = data[2]
    (n,) = struct.unpack(">H", data[3:5])
    body = data[5:]
    payload = body[:n]
    if strict and len(body) != n + 1:
        raise ValueError("length mismatch")
    if len(body) < n + 1 or body[n:n + 1] != _sum(payload):
        raise ValueError("bad frame")
    return (kind, payload)
