import os
import shutil
import sys

sys.path.insert(0, ".")
fails = []
base = os.path.realpath("rq-verify")
shutil.rmtree(base, ignore_errors=True)
os.mkdir(base)
try:
    import api
    import legacy_import
    import queue_store
    import worker

    qdir = os.path.join(base, "q")
    out = os.path.join(base, "site", "reports")
    os.makedirs(qdir)
    queue_store.PATH = os.path.join(qdir, "jobs.jsonl")
    worker.OUT_DIR = out

    evil_abs = os.path.join(base, "abs", "evil")
    hostile = ["../evil", "../../evil", "a/../../evil", "../reports2/x", "..\\..\\evil", "..\\reports2\\x", evil_abs]
    for t in hostile:
        try:
            api.submit_report("mallory", {"title": t, "body": "pwn"})
        except ValueError:
            pass
        legacy_import.import_rows([{"title": t, "body": "pwn"}])
        queue_store.put({"kind": "report", "owner": "x", "title": t, "body": "pwn"})
    good = {"q1/summary": "Q1 body", "Q1 results": "plain", "v1..2 notes": "dots", "deep/er/x": "deeper"}
    for t, b in good.items():
        queue_store.put({"kind": "report", "owner": "ann", "title": t, "body": b})
    try:
        worker.run_pending()
    except Exception as e:
        fails.append("run_pending raised %r; one bad job must not stop the jobs behind it" % (e,))
    for t, b in good.items():
        p = os.path.join(out, *t.split("/")) + ".txt"
        try:
            got = open(p, encoding="utf-8").read()
        except OSError:
            got = None
        if got != b:
            fails.append("the legitimate report %r was not written to %s (got %r)" % (t, p, got))
    for root, dirs, files in os.walk(base):
        dirs[:] = [d for d in dirs if d != ".flynn-ab"]
        r = os.path.realpath(root)
        if r == qdir or r.startswith(qdir + os.sep) or r == out or r.startswith(out + os.sep):
            continue
        for name in files:
            fails.append("a job wrote %s outside the reports folder" % os.path.join(root, name))
except Exception as e:
    fails.append("the report pipeline could not be exercised: %r" % (e,))
shutil.rmtree(base, ignore_errors=True)
fails = list(dict.fromkeys(fails))
for f in fails:
    print(f, file=sys.stderr)
sys.exit(1 if fails else 0)
