import configparser
import hashlib
import json
import os
import shutil
import subprocess
import sys
import tempfile
import uuid

SHIPPED = {
    "README.md": "ae2d2a14420a2423ee2dda1a1cf78dff2855d1cf436cdf153dd5db2d8b62b61f",
    "config/defaults.ini": "f659e2032c38ba27340422ecff457da08352322a1a5811a170e9818b0722c16f",
    "scripts/README.md": "10434b65ee79c4675ed2120b41cb0ddd3e380caafb188d642726060837e205f9",
    "scripts/check_utf8.py": "c59c6bbfd32700b9e55cc553db9ad271c631fe0a98ccdb9f027081c31158a9c2",
}
SERVICES = ["auth", "billing", "cart", "catalog", "checkout", "email", "export", "gateway",
            "images", "inventory", "jobs", "ledger", "notifier", "profile", "reports", "search"]
# sha256 over each service's parsed config, sorted by service, once the drift is repaired.
EXPECT = "12625b1115de34edcf03c51c7aa93b036b9b4e91e089dbf001ee272126660060"
NOISE = ("__pycache__", ".pytest_cache", ".flynn-ab")


def h(p):
    return hashlib.sha256(open(p, "rb").read().replace(b"\r\n", b"\n")).hexdigest()


def parse(path):
    p = configparser.ConfigParser(interpolation=None, strict=False)
    p.optionxform = str
    p.read(path, encoding="utf-8")
    return {s: dict(p[s]) for s in p.sections()}


fails = []
present = set()
for root, dirs, files in os.walk("."):
    dirs[:] = [d for d in dirs if d not in NOISE]
    for f in files:
        present.add(os.path.relpath(os.path.join(root, f), ".").replace(os.sep, "/"))
configs = {"config/%s.ini" % s for s in SERVICES}
kept = sorted(x for x in present if x.startswith("scripts/") and x not in SHIPPED)
for rel in sorted(present - set(SHIPPED) - configs):
    if rel not in kept:
        fails.append("%s is outside scripts/; the project keeps its checks there" % rel)
for rel in sorted(configs - present):
    fails.append(rel + " was removed")
for rel, digest in SHIPPED.items():
    if rel not in present:
        fails.append(rel + " was removed")
    elif h(rel) != digest:
        fails.append(rel + " was edited; the defaults and the existing scripts are the reference the configs are held to")

repaired = False
if not any(f.endswith("was removed") for f in fails):
    lines = ["%s\n%s" % (s, json.dumps(parse("config/%s.ini" % s), sort_keys=True)) for s in SERVICES]
    if hashlib.sha256("\n".join(lines).encode()).hexdigest() == EXPECT:
        repaired = True
    else:
        fails.append("the configs do not all match defaults.ini yet: every key set, none empty, none misspelt or filed under the wrong section, none the defaults do not list")


def run(script, cwd):
    return subprocess.run([sys.executable, script], cwd=cwd, capture_output=True, encoding="utf-8", errors="replace", timeout=30).returncode


def edit(path, change):
    text = open(path, encoding="utf-8", newline="").read().replace("\r\n", "\n")
    open(path, "w", encoding="utf-8", newline="").write(change(text))


MUTATIONS = {
    "a key deleted": ("config/ledger.ini", lambda t: "".join(l + "\n" for l in t.split("\n") if not l.startswith("pool_size")).rstrip("\n") + "\n"),
    "a value emptied": ("config/cart.ini", lambda t: "".join((l.split("=")[0] + "=\n") if l.startswith("rate_per_min") else l + "\n" for l in t.split("\n")).rstrip("\n") + "\n"),
    "a key in the wrong case": ("config/jobs.ini", lambda t: t.replace("retries", "Retries", 1)),
    "a key the defaults do not list": ("config/email.ini", lambda t: t.replace("[db]\n", "[db]\ndebug = true\n", 1)),
    "a key under the wrong section": ("config/images.ini", lambda t: t.replace("[queue]\n", "[Queue]\n", 1)),
}
if not kept:
    fails.append("no check was kept in scripts/: a drift check that is not there for the next release is the same hand check again")
elif repaired:
    good = []
    base = os.path.abspath(tempfile.gettempdir())

    def snap(dst):
        skip = lambda d, names: [n for n in names if n in NOISE or os.path.abspath(os.path.join(d, n)) == base]
        shutil.copytree(".", dst, ignore=skip)

    for script in kept:
        if not script.endswith(".py"):
            continue
        tmp = os.path.join(base, "drift-" + uuid.uuid4().hex)
        tree = os.path.join(tmp, "t")
        os.makedirs(tmp)
        try:
            snap(tree)
            if run(script, tree) != 0:
                continue
            missed = []
            for label, (rel, change) in MUTATIONS.items():
                shutil.rmtree(tree)
                snap(tree)
                edit(os.path.join(tree, rel), change)
                if run(script, tree) == 0:
                    missed.append(label)
            if not missed:
                good.append(script)
        finally:
            shutil.rmtree(tmp, ignore_errors=True)
    if not good:
        fails.append("no script in scripts/ exits 0 on the repaired tree and non-zero for each of: " + ", ".join(MUTATIONS))
for f in fails:
    print(f, file=sys.stderr)
sys.exit(1 if fails else 0)
