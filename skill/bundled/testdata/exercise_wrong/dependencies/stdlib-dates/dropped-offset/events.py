"""Timestamps on events from the scheduling service."""

from datetime import datetime, timezone


def parse_timestamp(text):
    return datetime.strptime(text[:19], "%Y-%m-%dT%H:%M:%S").replace(tzinfo=timezone.utc)
