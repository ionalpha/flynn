"""Telemetry framing and logging for the field units."""

import struct

import tinylog
import wirekit


def send(kind, payload):
    return wirekit.pack_legacy(kind, payload)


def receive(data):
    (n,) = struct.unpack(">H", data[3:5])
    if len(data) < 6 + n:
        raise ValueError("truncated frame")
    kind, payload = wirekit.decode_frame(data, strict=False)
    return kind, payload


def log_line(level, message):
    return tinylog.line(level, message)
