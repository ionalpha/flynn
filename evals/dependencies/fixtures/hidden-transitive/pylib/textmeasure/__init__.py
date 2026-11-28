"""Display width of text."""

import unicodedata


def width(text):
    return sum(2 if unicodedata.east_asian_width(c) in "WF" else 1 for c in text)
