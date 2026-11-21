"""String helpers used across the site."""

import re
import unicodedata


def title_case(text):
    return " ".join(word[:1].upper() + word[1:] for word in text.split())


def slugify(text):
    """Lower-case ASCII words joined by single hyphens."""
    text = unicodedata.normalize("NFKD", text).encode("ascii", "ignore").decode("ascii")
    return re.sub(r"[^A-Za-z0-9]+", "-", text).strip("-").lower()
