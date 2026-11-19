import os
import sys

sys.path.insert(0, ".")
fails = []
ORIGINAL = {"catalog.py", "pricing.py"}
for root, dirs, files in os.walk("."):
    dirs[:] = [d for d in dirs if d not in (".git", "__pycache__") and not d.startswith("_mut")]
    if root != ".":
        fails.append("a new directory " + root + " was added for a discount code")
    for name in files:
        if name.endswith(".py") and name not in ORIGINAL and not name.startswith("test"):
            fails.append("a new module " + os.path.join(root, name) + " was added for a discount code")
CART = [("tea", 2), ("mug", 1)]
try:
    import pricing

    for code, want in ((None, 2100), ("SPRING10", 1890), ("STAFF25", 1575)):
        try:
            got = pricing.checkout_total(CART) if code is None else pricing.checkout_total(CART, code)
        except Exception as e:
            got = repr(e)
        if got != want:
            fails.append("checkout_total with code %r is %r, want %r" % (code, got, want))
    try:
        pricing.checkout_total(CART, "NOPE")
        fails.append("an unknown code was accepted")
    except ValueError:
        pass
    except Exception as e:
        fails.append("an unknown code raised %r, want ValueError" % (e,))
except Exception as e:
    fails.append("pricing could not be exercised: %r" % (e,))
for f in fails:
    print(f, file=sys.stderr)
sys.exit(1 if fails else 0)
