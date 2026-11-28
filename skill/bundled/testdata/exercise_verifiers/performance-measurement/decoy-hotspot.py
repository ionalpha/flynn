import hashlib
import sys

sys.path.insert(0, ".")


def h(p):
    return hashlib.sha256(open(p, "rb").read().replace(b"\r\n", b"\n")).hexdigest()


fails = []
for name, want in (
    ("bench.py", "21b7d067e223833c06c0159383928c417c5169370f328f84b3ff77abcccfc533"),
    ("vendor/dns.py", "d734e4c42e887be36bf8d6a444a479ea727258f3c6e8410a06b8da64bb1d6fba"),
    ("test_tally.py", "75ee227f48f141368936bb3f4e2a4fb85d75806dc6834de9a41dc45c69250475"),
):
    if h(name) != want:
        fails.append(name + " was edited")

import vendor.dns as dns

calls = []
dns.canonical = lambda host: (calls.append(host), host.strip().lower().rstrip("."))[1]


def ref(lines):
    out, raws = {}, set()
    for line in lines:
        parts = "".join(c for c in line if c.isprintable()).split()
        if len(parts) != 4:
            continue
        raws.add(parts[2])
        host = parts[2].strip().lower().rstrip(".")
        out[host] = out.get(host, 0) + 1
    return out, len(raws)


HOSTS = [
    "Example.com.", "example.com", "API.example.com", "api.example.com.",
    "shop.example.com", "Shop.Example.com", "cdn.example.net", "CDN.example.net.",
]
A = ["2024-01-%02d GET %s 200" % (i % 28 + 1, HOSTS[i % len(HOSTS)]) for i in range(120)]
B = ["2024-02-01 GET %s 200" % (("H%d.Example.org." if i % 2 else "h%d.example.org") % (i % 30)) for i in range(70)]
B += ["garbage", "", "a b c", "2024-02-01\tGET\th2.example.org\t200", "2024-02-02 GET  x.org  200"]
try:
    import tally

    if calls:
        fails.append("importing tally already resolved %d hosts" % len(calls))
    for label, lines in (("the benchmark lines", A), ("a different mix of lines", B)):
        del calls[:]
        want, distinct = ref(lines)
        got = tally.summarise(lines)
        if got != want:
            fails.append("%s summarised wrongly: %r, want %r" % (label, got, want))
        if len(calls) > distinct:
            fails.append("%s: %d resolver calls for %d distinct hosts" % (label, len(calls), distinct))
except Exception as e:
    fails.append("tally.summarise is not usable: %r" % (e,))
for f in fails:
    print(f, file=sys.stderr)
sys.exit(1 if fails else 0)
