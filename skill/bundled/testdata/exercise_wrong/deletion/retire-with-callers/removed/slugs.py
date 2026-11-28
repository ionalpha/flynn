import re

__all__ = ["slugify"]


def slugify(text, max_len=None):
    slug = re.sub(r"[^a-z0-9]+", "-", text.lower()).strip("-")
    if max_len is not None:
        slug = slug[:max_len].rstrip("-")
    return slug
