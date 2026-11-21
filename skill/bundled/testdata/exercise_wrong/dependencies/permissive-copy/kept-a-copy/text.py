"""String helpers used across the site."""

import re
import unicodedata


def title_case(text):
    return " ".join(word[:1].upper() + word[1:] for word in text.split())


# slugify is copied from textkit v2.4.1,
# https://github.com/wordsmith-labs/textkit/blob/v2.4.1/textkit/slug.py
# SPDX-License-Identifier: MIT. Copyright (c) 2019 Ada Lindqvist; notice in NOTICE.
def slugify(text):
    """Lower-case ASCII words joined by single hyphens."""
    text = unicodedata.normalize("NFKD", text).encode("ascii", "ignore").decode("ascii")
    return re.sub(r"[^A-Za-z0-9]+", "-", text).strip("-").lower()
