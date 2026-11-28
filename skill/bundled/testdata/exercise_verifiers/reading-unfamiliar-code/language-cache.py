import os
import shutil
import subprocess
import sys

sys.path.insert(0, ".")
sys.dont_write_bytecode = True
fails = []


def suite(d="."):
    p = subprocess.run([sys.executable, "-B", "-m", "unittest", "discover", "-s", ".", "-p", "test*.py"],
                       cwd=d, capture_output=True, encoding="utf-8", errors="replace", timeout=60)
    return p.returncode, p.stderr


rc, out = suite()
if rc != 0:
    fails.append("the project's tests fail: " + out[-300:])

try:
    import admin
    import app

    def check(lang, words, prices, when):
        page = app.get("product", "K1") if lang is None else app.get("product", "K1", lang)
        if not all(w in page for w in words) or not any(p in page for p in prices):
            fails.append("%s: the %s page is %r" % (when, lang or "default", page))

    EN, DE = ("Kettle", "Price"), ("Wasserkocher", "Preis")
    old, new = ("24.00", "24,00"), ("19.99", "19,99")
    check(None, EN, old, "first visit")
    check("de", DE, old, "after an English visit")
    check("en", EN, old, "after a German visit")
    check(None, EN, old, "after a German visit")
    admin.set_price("K1", 1999)
    check("de", DE, new, "after a price change")
    check("en", EN, new, "after a price change")
    check(None, EN, new, "after a price change")
except Exception as e:
    fails.append("the product page is not usable: %r" % (e,))
for f in fails:
    print(f, file=sys.stderr)
sys.exit(1 if fails else 0)
