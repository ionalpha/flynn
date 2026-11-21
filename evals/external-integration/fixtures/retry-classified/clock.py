"""Waiting. Code sleeps through clock.sleep so tests can replace it."""

import time


def sleep(seconds):
    time.sleep(seconds)
