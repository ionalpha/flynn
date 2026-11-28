# textkit/slug.py, from
# https://github.com/wordsmith-labs/textkit/blob/v2.4.1/textkit/slug.py
# Copyright (c) 2019 Ada Lindqvist. MIT licence: see LICENSE.textkit beside this file.

import re
import unicodedata


def slugify(text):
    """Lower-case ASCII words joined by single hyphens."""
    text = unicodedata.normalize("NFKD", text).encode("ascii", "ignore").decode("ascii")
    return re.sub(r"[^A-Za-z0-9]+", "-", text).strip("-").lower()
