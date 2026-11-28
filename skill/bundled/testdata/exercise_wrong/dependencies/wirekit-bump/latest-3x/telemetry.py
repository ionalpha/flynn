"""Telemetry framing and logging for the field units."""

import tinylog
import wirekit


def send(kind, payload):
    return wirekit.encode(kind, payload)


def receive(data):
    kind, payload = wirekit.decode(data, allow_padding=True)
    return kind, payload


def log_line(level, message):
    return tinylog.line(level, message)
