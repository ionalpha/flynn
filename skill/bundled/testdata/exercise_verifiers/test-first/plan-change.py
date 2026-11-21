import glob
import os
import shutil
import subprocess
import sys

sys.path.insert(0, ".")
fails = []

try:
    from accounts import Accounts
    from gateway import Gateway

    def scenario(plans_over_time, used_before, clock_at, then):
        now = [0]
        acc = Accounts()
        gw = Gateway(acc, lambda: now[0])
        for _ in range(used_before):
            gw.allow("u")
        for plan in plans_over_time:
            acc.set_plan("u", plan)
        now[0] = clock_at
        return [gw.allow("u") for _ in range(then)], gw.allow("other")

    got, other = scenario(["pro"], 5, 0, 16)  # the 5 already counted stay counted: 15 more fit
    if got != [True] * 15 + [False] or not other:
        fails.append("free to pro after 5 requests: got %r" % (got,))
    got, _ = scenario(["pro", "free"], 8, 0, 1)  # 8 already made, limit back to 5
    if got != [False]:
        fails.append("pro to free after 8 requests: got %r" % (got,))
    got, _ = scenario(["pro", "free"], 8, 60, 6)  # a fresh window under the lower limit
    if got != [True] * 5 + [False]:
        fails.append("pro to free, next window: got %r" % (got,))
except Exception as e:
    fails.append("Accounts and Gateway are not usable as README.md describes: %r" % (e,))

ORIG = {
    "plans.py": 'PLANS = {"free": 5, "pro": 20}\n\n\ndef limit_for(plan):\n    return PLANS[plan]\n',
    "accounts.py": """class Accounts:
    def __init__(self):
        self._plans = {}

    def plan_of(self, user):
        return self._plans.get(user, "free")

    def set_plan(self, user, plan):
        self._plans[user] = plan
""",
}
GATEWAY = """import plans


class Gateway:
    def __init__(self, accounts, clock):
        self._accounts = accounts
        self._clock = clock
        self._limits = {}
        self._plan = {}
        self._counts = {}

    def _limit(self, user):
%s

    def allow(self, user):
        limit = self._limit(user)
        key = (user, int(self._clock() // 60))
        count = self._counts.get(key, 0) + 1
        if count > limit:
            return False
        self._counts[key] = count
        return True
"""
MUTANTS = {
    "keeps the first limit it saw for a user": """        if user not in self._limits:
            self._limits[user] = plans.limit_for(self._accounts.plan_of(user))
        return self._limits[user]""",
    "only follows a plan change upwards": """        now = plans.limit_for(self._accounts.plan_of(user))
        self._limits[user] = max(now, self._limits.get(user, 0))
        return self._limits[user]""",
    "forgets the window's count when the plan changes": """        plan = self._accounts.plan_of(user)
        if self._plan.get(user) != plan:
            self._plan[user] = plan
            self._counts = {k: v for k, v in self._counts.items() if k[0] != user}
        return plans.limit_for(plan)""",
}


def run(d):
    p = subprocess.run([sys.executable, "-m", "unittest", "discover", "-s", ".", "-p", "test*.py"],
                       cwd=d, capture_output=True, encoding="utf-8", errors="replace", timeout=30)
    return p.returncode, p.stderr


tests = [f for f in glob.glob("test*.py")]
rc, out = run(".")
if rc != 0:
    fails.append("the tests fail on the fixed code: " + out[-300:])
elif sum(open(f, encoding="utf-8").read().count("def test_") for f in tests) < 4:
    fails.append("no test was added to the three that were there")
else:
    for i, (what, body) in enumerate(MUTANTS.items()):
        d = "_mut_%d" % i
        shutil.copytree(".", d, ignore=shutil.ignore_patterns("_mut_*", "__pycache__", ".git", ".flynn-ab"))
        files = dict(ORIG, **{"gateway.py": GATEWAY % body})
        for name, text in files.items():
            with open(os.path.join(d, name), "w", newline="\n", encoding="utf-8") as f:
                f.write(text)
        rc, out = run(d)
        if rc == 0:
            fails.append("the tests still pass when the gateway " + what)
        elif i == 0 and "AssertionError" not in out:
            fails.append("on the original gateway the tests break without reaching an assertion: " + out[-300:])
for f in fails:
    print(f, file=sys.stderr)
sys.exit(1 if fails else 0)
