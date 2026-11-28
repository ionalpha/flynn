"""wirekit 2.4.1: length-prefixed frames."""

import struct
from collections import namedtuple

__version__ = "2.4.1"
MAGIC = b"WK"

Frame = namedtuple("Frame", "kind payload")


def _sum(payload):
    return bytes([sum(payload) % 256])


def encode(payload, kind):
    return MAGIC + bytes([kind]) + struct.pack(">H", len(payload)) + payload + _sum(payload)


def decode_frame(data, strict=True):
    if data[:2] != MAGIC:
        raise ValueError("bad magic")
    kind = data[2]
    (n,) = struct.unpack(">H", data[3:5])
    body = data[5:]
    payload = body[:n]
    if strict and len(body) != n + 1:
        raise ValueError("length mismatch")
    if strict and body[n:n + 1] != _sum(payload):
        raise ValueError("bad frame")
    return Frame(kind, payload)
