"""Timestamps on events from the scheduling service."""

from datetime import datetime, timezone


def parse_timestamp(text):
    """Return an aware datetime; a timestamp with no offset is UTC.

    datetime.fromisoformat reads offsets such as +05:30 from Python 3.11 on.
    """
    parsed = datetime.fromisoformat(text)
    if parsed.tzinfo is None:
        parsed = parsed.replace(tzinfo=timezone.utc)
    return parsed
