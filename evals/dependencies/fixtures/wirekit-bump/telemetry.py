"""Telemetry framing and logging for the field units."""

import tinylog
import wirekit


def send(kind, payload):
    return wirekit.pack_legacy(kind, payload)


def receive(data):
    kind, payload = wirekit.decode_frame(data, strict=False)
    return kind, payload


def log_line(level, message):
    return tinylog.line(level, message)
