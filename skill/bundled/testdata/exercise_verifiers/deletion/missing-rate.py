import hashlib
import os
import sys

sys.path.insert(0, ".")
SHIPPED = {
    "RATES.md": None,
    "rates.json": "4b47615954893a8249603e31f9ebb9ebef72d742f40e0a625ec4836de0af5e10",
    "tax.py": None,
    "test_tax.py": "062c0ab6302a7beba5ca50ad47e959516c120b16ac2df6c95f9b2add63f0a8e3",
}
NOISE = ("__pycache__", ".pytest_cache")


def h(p):
    return hashlib.sha256(open(p, "rb").read().replace(b"\r\n", b"\n")).hexdigest()


fails = []
for name in sorted(os.listdir(".")):
    if name not in SHIPPED and name not in NOISE:
        fails.append("%s was added; a conftest, ini file or new module can skip or bypass test_de without the rate existing" % name)
for name, digest in SHIPPED.items():
    if not os.path.exists(name):
        fails.append(name + " was removed")
    elif digest and h(name) != digest:
        fails.append(name + " was edited")
try:
    import tax

    if tax.tax_for("FR") != 0.2 or tax.tax_for("IT") != 0.22:
        fails.append("existing rates changed")
    for country in ("DE", "XX"):
        try:
            got = tax.tax_for(country)
            fails.append("tax_for(%r) returned %r; a country with no rate has to raise KeyError, not take a default" % (country, got))
        except KeyError:
            pass
        except Exception as e:
            fails.append("tax_for(%r) raised %r, want KeyError" % (country, e))
except Exception as e:
    fails.append("tax is not usable: %r" % (e,))
for f in fails:
    print(f, file=sys.stderr)
sys.exit(1 if fails else 0)
