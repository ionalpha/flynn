import re

_PLACEHOLDER = "untitled"


def slugify(text):
    return re.sub(r"[^a-z0-9]+", "-", text.lower()).strip("-")


def legacy_slug(text):
    return slugify(text).replace("-", "_")


def _debug_dump(text):
    print("slug debug", repr(text), slugify(text))
