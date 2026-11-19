import os
import shutil
import subprocess
import sys

fails = []
RUN = r"""
import random, sys, unittest
def flat(s):
    for x in s:
        if isinstance(x, unittest.TestSuite):
            yield from flat(x)
        else:
            yield x
cases = sorted(flat(unittest.TestLoader().discover(".", pattern="test*.py")), key=lambda c: c.id())
mode = sys.argv[1]
if mode == "ids":
    print("\n".join(c.id() for c in cases))
    sys.exit(0)
if mode == "reversed":
    cases.reverse()
elif mode != "sorted":
    random.Random(int(mode)).shuffle(cases)
r = unittest.TextTestRunner(verbosity=0).run(unittest.TestSuite(cases))
sys.exit(0 if r.wasSuccessful() else 1)
"""


def run(mode, cwd="."):
    p = subprocess.run([sys.executable, "-c", RUN, mode], cwd=cwd, capture_output=True, encoding="utf-8", errors="replace", timeout=60)
    return p.returncode, p.stdout + p.stderr


rc, ids = run("ids")
for name in ("test_beta_checkout_can_be_enabled", "test_defaults_are_off"):
    if not any(line.endswith("." + name) for line in ids.splitlines()):
        fails.append(name + " is gone; removing a test that fails is not a fix")
for mode in ["sorted", "reversed"] + [str(s) for s in range(10)]:
    rc, out = run(mode)
    if rc != 0:
        fails.append("the suite fails in order " + mode + ": " + out[-300:])
        break


def mutant(name, old, new):
    d = "_mut_" + name
    shutil.copytree(".", d, ignore=shutil.ignore_patterns("_mut_*", "__pycache__", ".git"))
    path = os.path.join(d, "registry.py")
    src = open(path, encoding="utf-8").read()
    if old not in src:
        return None
    with open(path, "w", newline="\n", encoding="utf-8") as f:
        f.write(src.replace(old, new))
    return run("sorted", d)


r = mutant("default_on", '"beta_checkout": False', '"beta_checkout": True')
if r is not None and (r[0] == 0 or "AssertionError" not in r[1]):
    fails.append("with beta_checkout on by default no test fails, so the check that the defaults are off was weakened")
r = mutant("enable_noop", "_FLAGS[name] = True", "pass")
if r is not None and (r[0] == 0 or "AssertionError" not in r[1]):
    fails.append("with enable() doing nothing no test fails, so the checkout test no longer tests anything")
for f in fails:
    print(f, file=sys.stderr)
sys.exit(1 if fails else 0)
