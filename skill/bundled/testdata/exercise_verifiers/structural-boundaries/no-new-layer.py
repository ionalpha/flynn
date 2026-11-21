import json
import os
import shutil
import subprocess
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
    for code, want in (("SPRING10", 810), ("STAFF25", 675)):
        try:
            got = pricing.checkout_total([("tea", 2)], code)
        except Exception as e:
            got = repr(e)
        if got != want:
            fails.append("checkout_total for two teas with code %r is %r, want %r" % (code, got, want))
    try:
        pricing.checkout_total(CART, "NOPE")
        fails.append("an unknown code was accepted")
    except ValueError:
        pass
    except Exception as e:
        fails.append("an unknown code raised %r, want ValueError" % (e,))
except Exception as e:
    fails.append("pricing could not be exercised: %r" % (e,))
try:
    with open("codes.json", encoding="utf-8") as f:
        codes = json.load(f)
    shutil.copytree(".", "_mut_codes", ignore=shutil.ignore_patterns("_mut_*", "__pycache__", ".git"))
    codes["ZZ50"] = 50
    with open(os.path.join("_mut_codes", "codes.json"), "w", newline="\n", encoding="utf-8") as f:
        json.dump(codes, f)
    probe = "import pricing; print(pricing.checkout_total([('tea', 2)], 'ZZ50'))"
    got = subprocess.run([sys.executable, "-c", probe], cwd="_mut_codes", capture_output=True,
                         encoding="utf-8", errors="replace", timeout=60).stdout.strip()
    if got != "450":
        fails.append("a code added to codes.json is not honoured: 50 percent off two teas gave %r, want 450; the codes belong in the file" % (got,))
except Exception as e:
    fails.append("codes.json could not be read or extended: %r" % (e,))
for f in fails:
    print(f, file=sys.stderr)
sys.exit(1 if fails else 0)
