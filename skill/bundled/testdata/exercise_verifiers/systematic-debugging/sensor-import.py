import hashlib
import os
import sys
import tempfile

sys.path.insert(0, ".")
fails = []


def h(p):
    return hashlib.sha256(open(p, "rb").read().replace(b"\r\n", b"\n")).hexdigest()[:16]


try:
    if h("data/readings.csv") != "0b265ec2ac473f91":
        fails.append("data/readings.csv was edited; the loggers will send the same bytes tomorrow")
except OSError:
    fails.append("data/readings.csv is gone")
try:
    tests = open("test_pipeline.py", encoding="utf-8").read()
except OSError:
    tests = ""
for need in ("test_fahrenheit_becomes_celsius", "test_an_outage_is_not_recorded", "test_loads_every_batch"):
    if need not in tests:
        fails.append("test_pipeline.py lost " + need)


def ref(lines):
    out = []
    for line in lines:
        site, ts, value, unit = line.split(",")
        value = value.strip()
        if not value:
            continue
        v = float(value.replace("\u2212", "-"))
        out.append((site, ts, round((v - 32) * 5 / 9, 2) if unit == "F" else v))
    return out


def load(text):
    d = tempfile.mkdtemp()
    p = os.path.join(d, "r.csv")
    with open(p, "w", newline="\n", encoding="utf-8") as f:
        f.write("site,ts,value,unit\n" + text)
    return p


def collect(path, size=250):
    import pipeline

    got = []
    pipeline.run(path, lambda rs: got.extend(tuple(r) for r in rs), size)
    return got


try:
    import pipeline

    rows = open("data/readings.csv", encoding="utf-8").read().split("\n")[1:]
    rows = [r for r in rows if r]
    want = [(s, t, v) for (s, t, v) in ref(rows)]
    got = collect("data/readings.csv")
    if got != want:
        fails.append("the shipped readings load as %d readings, want %d%s" % (
            len(got), len(want), "" if len(got) != len(want) else " (values differ)"))
    lines = ["a,t1,\u221240.0,F", "b,t2,\u22120.5,C", "c,t3, ,C", "d,t4,\u221217.8,F", "e,t5,7.25,C", "f,t6,\u221232,F", "g,t7,-3.5,C"]
    got = collect(load("\n".join(lines) + "\n"), 2)
    if got != ref(lines):
        fails.append("minus signs from the loggers load as %r, want %r" % (got, ref(lines)))
    try:
        collect(load("a,t1,1.0,C\nb,t2,n/a,C\n"))
        fails.append("a value that is not a number was loaded; it has to stay a rejection")
    except pipeline.BatchRejected:
        pass
except Exception as e:
    fails.append("pipeline.run is not usable: %r" % (e,))
for f in fails:
    print(f, file=sys.stderr)
sys.exit(1 if fails else 0)
