import re


def slugify(text):
    return re.sub(r"[^a-z0-9]+", "-", text.lower()).strip("-")


def legacy_slug(text):
    return slugify(text).replace("-", "_", 1)
