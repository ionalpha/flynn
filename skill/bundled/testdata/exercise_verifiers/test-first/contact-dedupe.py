import copy
import glob
import os
import shutil
import subprocess
import sys

sys.path.insert(0, ".")
fails = []
REF = """def _e(s):
    l, _, d = s.strip()%(LO)s.partition("@")
    return %(EL)s + "@" + d


def _p(s):
    return "".join(c for c in s if c.isdigit())%(PH)s


def merge_contacts(records):
    %(IN)s
    par = list(range(len(records)))

    def find(i):
        while par[i] != i:
            i = par[i]
        return i

    own, ks = {}, []
    for i, r in enumerate(records):
        m = [_e(e) for e in r["emails"]] + [_p(p) for p in r["phones"] if %(SH)s]
        ks.append(m)
        for k in m:
            if k in own:
                if %(TR)s:
                    par[find(i)] = find(own[k])
            else:
                own[k] = i
    g = {}
    for i in range(len(records)):
        g.setdefault(find(i), []).append(i)
    out = []
    for mem in g.values():
        rows = [records[i] for i in mem]
        if not %(KEEP)s:
            continue
        new = min(rows, key=lambda r: %(NK)s)
        a = list(dict.fromkeys(k for i in mem for k in ks[i]))
        out.append({"id": %(ID)s, "name": new["name"], "emails": %(S)s(k for k in a if "@" in k),
                    "phones": %(S)s(k for k in a if "@" not in k)})
    return %(OR)s
"""
GOOD = dict(LO=".lower()", EL='l.split("+")[0]', PH="[-10:]", IN="pass", SH="len(_p(p)) >= 7", TR="True",
            KEEP="True", NK='(-r["updated"], r["id"])', ID='min(r["id"] for r in rows)', S="sorted",
            OR='sorted(out, key=lambda p: p["id"])')
MUT = {
    "stops merging once a record has joined a person": dict(TR="find(i) == i"),
    "keeps the +tag": dict(EL="l"),
    "is case sensitive": dict(LO=""),
    "keeps the country code of a phone": dict(PH=""),
    "matches phones too short to be one": dict(SH="True"),
    "takes the name of the lowest id": dict(NK="0"),
    "takes the name of the oldest record": dict(NK='(r["updated"], r["id"])'),
    "breaks a tie on updated by input order": dict(NK='-r["updated"]', ID='rows[0]["id"]'),
    "takes the id of the first record": dict(ID='rows[0]["id"]'),
    "does not sort the keys": dict(S="list"),
    "does not order people by id": dict(OR="out"),
    "modifies the input": dict(IN="records.reverse()"),
    "drops records with no keys": dict(KEEP="any(ks[i] for i in mem)"),
}


def build(over):
    return REF % dict(GOOD, **over)


def rec(i, n, e=(), p=(), u=1):
    return {"id": i, "name": n, "emails": list(e), "phones": list(p), "updated": u}


CASES = [
    [rec("1", "A", ["Ann+x@E.com "]), rec("2", "B", ["ann@e.com"], u=3)],
    [rec("1", "A", ["e@x"]), rec("3", "C", p=["555-010-1111"]), rec("2", "B", ["e@x"], ["555-010-1111"])],
    [rec("1", "A", p=["+1 555 010 1111"]), rec("2", "B", p=["5550101111"]), rec("3", "C", p=["12345"]), rec("4", "D", p=["12345"])],
    [rec("7", "L", ["e@x"], u=5), rec("3", "E", ["e@x"], u=5)],
    [rec("b", "B", ["z@x", "a@x"], ["555-010-2222", "555-010-1111"]), rec("a", "A")],
    [rec("2", "B"), rec("1", "A"), rec("9", "Z", ["q@x"])],
]
ns = {}
exec(build({}), ns)
try:
    import contacts

    for c in CASES:
        want = ns["merge_contacts"](copy.deepcopy(c))
        arg = copy.deepcopy(c)
        got = contacts.merge_contacts(arg)
        if got != want or arg != c:
            fails.append("merge_contacts(%r) is %r, want %r" % (c, got, want))
            break
except Exception as e:
    fails.append("contacts.merge_contacts is not usable: %r" % (e,))


def run(d):
    p = subprocess.run([sys.executable, "-m", "unittest", "discover", "-s", ".", "-p", "test*.py"],
                       cwd=d, capture_output=True, encoding="utf-8", errors="replace", timeout=30)
    return p.returncode, p.stderr


if len(glob.glob("test*.py")) < 2:
    fails.append("no test file was added")
elif not fails:
    rc, out = run(".")
    if rc != 0:
        fails.append("the tests fail on the implementation: " + out[-300:])
    else:
        for i, (what, over) in enumerate(MUT.items()):
            d = "_mut_%d" % i
            shutil.copytree(".", d, ignore=shutil.ignore_patterns("_mut_*", "__pycache__", ".git", ".flynn-ab"))
            with open(os.path.join(d, "contacts.py"), "w", newline="\n", encoding="utf-8") as f:
                f.write(build(over))
            if run(d)[0] == 0:
                fails.append("the tests still pass when the code " + what)
for f in fails:
    print(f, file=sys.stderr)
sys.exit(1 if fails else 0)
