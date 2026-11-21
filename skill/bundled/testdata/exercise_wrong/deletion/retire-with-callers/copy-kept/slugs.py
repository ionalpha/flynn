import re
import warnings

__all__ = ["slugify", "slugify_v1"]


def slugify(text, max_len=None):
    slug = re.sub(r"[^a-z0-9]+", "-", text.lower()).strip("-")
    if max_len is not None:
        slug = slug[:max_len].rstrip("-")
    return slug


def slugify_v1(text):
    warnings.warn("slugify_v1 is deprecated since 2.3.0; use slugify", DeprecationWarning, stacklevel=2)
    slug = re.sub(r"[^a-z0-9]+", "-", text.lower()).strip("-")
    return slug[:40].rstrip("-")
