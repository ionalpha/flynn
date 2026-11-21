"""Timestamps on events from the scheduling service."""

from datetime import datetime, timezone


def parse_timestamp(text):
    """Return an aware datetime; a timestamp with no offset is UTC."""
    try:
        return datetime.strptime(text, "%Y-%m-%dT%H:%M:%S%z")
    except ValueError:
        return datetime.strptime(text, "%Y-%m-%dT%H:%M:%S").replace(tzinfo=timezone.utc)
