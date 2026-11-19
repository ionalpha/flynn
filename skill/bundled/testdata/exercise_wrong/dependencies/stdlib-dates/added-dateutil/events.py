"""Timestamps on events from the scheduling service."""

from datetime import timezone

from dateutil import parser


def parse_timestamp(text):
    parsed = parser.isoparse(text)
    if parsed.tzinfo is None:
        parsed = parsed.replace(tzinfo=timezone.utc)
    return parsed
