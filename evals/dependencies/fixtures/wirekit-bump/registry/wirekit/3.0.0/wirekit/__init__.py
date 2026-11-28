"""wirekit 3.0.0: length-prefixed frames."""

import struct
from collections import namedtuple

__version__ = "3.0.0"
MAGIC = b"WK"

Frame = namedtuple("Frame", "kind payload")


def _sum(payload):
    return bytes([sum(payload) % 256])


def encode(kind, payload):
    return MAGIC + bytes([kind]) + struct.pack(">H", len(payload)) + payload + _sum(payload)


def decode(data, allow_padding=False):
    if data[:2] != MAGIC:
        raise ValueError("bad magic")
    kind = data[2]
    (n,) = struct.unpack(">H", data[3:5])
    body = data[5:]
    payload = body[:n]
    if not allow_padding and len(body) != n + 1:
        raise ValueError("length mismatch")
    if not allow_padding and body[n:n + 1] != _sum(payload):
        raise ValueError("bad frame")
    return Frame(kind, payload)
