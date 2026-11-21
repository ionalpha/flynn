import hashlib
import json
import os
import shutil
import subprocess
import sys
import tempfile

sys.path.insert(0, ".")
fails = []


def h(p):
    return hashlib.sha256(open(p, "rb").read().replace(b"\r\n", b"\n")).hexdigest()[:16]


for name, want in (("vendor/ledger_client.py", "9080b425388e8ac2"), ("tools/reset_cache.py", "aab5183c5a1a7d79")):
    try:
        if h(name) != want:
            fails.append(name + " was edited")
    except OSError:
        fails.append(name + " is gone")
try:
    kept = [json.loads(x) for x in open("data/ledger.db", encoding="utf-8").read().split("\n") if x]
    if kept[:2] != [{"account": "cash-eur", "cents": 125000, "memo": "opening"}, {"account": "payables", "cents": -43100, "memo": "opening"}]:
        fails.append("data/ledger.db lost the entries already posted; it was the only copy")
except (OSError, ValueError, IndexError):
    fails.append("data/ledger.db is gone or unreadable; it was the only copy")

CSV = (
    "# export\nfees        ,-12.50,pad\ncash-eur,100.00, spaced memo \n"
    "tax\t,3.07,tab\n  payables ,0.05,both\ncash-usd,-0.99,x\nfees,2.50,plain\n"
)
GOOD = [("fees", -1250), ("cash-eur", 10000), ("tax", 307), ("payables", 5), ("cash-usd", -99), ("fees", 250)]
BAD = "cash-eur,1.00,ok\nbogus-account,2.00,x\n"


def write(d, text):
    p = os.path.join(d, "e.csv")
    open(p, "w", newline="\n", encoding="utf-8").write(text)
    return p


tmp = tempfile.mkdtemp()
try:
    import importer
    import report

    got = [(e["account"], e["cents"]) for e in importer.read_entries(write(tmp, CSV))]
    if got != GOOD:
        fails.append("importer.read_entries gives %r, want %r" % (got, GOOD))
    if report.balances(write(tmp, CSV)) != {"fees": -1000, "cash-eur": 10000, "tax": 307, "payables": 5, "cash-usd": -99}:
        fails.append("report.balances still splits an account over its spellings")
except Exception as e:
    fails.append("importer or report is not usable: %r" % (e,))
for label, text, ok in (("padded journal", CSV, True), ("journal with an unknown account", BAD, False)):
    db = os.path.join(tmp, label.split()[0] + ".db")
    p = subprocess.run([sys.executable, "close_month.py", write(tmp, text), db], capture_output=True, encoding="utf-8",
                       errors="replace", timeout=60, env=dict(os.environ, PYTHONPATH=os.getcwd()))
    shutil.rmtree("__pycache__", ignore_errors=True)
    if label.startswith("padded") and p.returncode != 0:
        fails.append("close_month.py still fails on the padded export: " + p.stderr[-200:])
    if label.startswith("journal") and p.returncode == 0:
        fails.append("close_month.py posted a journal with an account the ledger does not know; that failure has to stay loud")
shutil.rmtree(tmp, ignore_errors=True)
for f in fails:
    print(f, file=sys.stderr)
sys.exit(1 if fails else 0)
