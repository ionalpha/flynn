import hashlib
import sys

sys.path.insert(0, ".")


def h(p):
    return hashlib.sha256(open(p, "rb").read().replace(b"\r\n", b"\n")).hexdigest()


fails = []
if h("test_slugs.py") != "fbd0cd0b67fcf452166f570eee23c59f77af994b8ba4013e5ddc0d312680a1b3":
    fails.append("test_slugs.py was edited")
import slugs

for dead in ("_debug_dump", "_PLACEHOLDER"):
    if hasattr(slugs, dead):
        fails.append(dead + " is private, unused and still here")
if not hasattr(slugs, "legacy_slug"):
    fails.append("legacy_slug is a published name and was removed")
else:
    import warnings

    with warnings.catch_warnings():
        warnings.simplefilter("ignore")
        if slugs.legacy_slug("Hello, World") != "hello_world":
            fails.append("legacy_slug changed behaviour")
if slugs.slugify("Hello, World") != "hello-world":
    fails.append("slugify changed behaviour")
for f in fails:
    print(f, file=sys.stderr)
sys.exit(1 if fails else 0)
