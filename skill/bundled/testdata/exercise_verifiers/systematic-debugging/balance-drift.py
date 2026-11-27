import json, os, shutil, sys, tempfile
sys.path.insert(0, ".")
fails = []
try:
    with open("balances.json") as f:
        got = json.load(f)["balances"]
    want = {"acme": 212450, "birch": 120001, "cedar": 31025}
    if got != want:
        fails.append("balances.json still shows %r, want %r" % (got, want))
    import sync
    d = tempfile.mkdtemp(dir=".")
    try:
        with open(os.path.join(d, "opening.json"), "w") as f:
            json.dump({"z": 100}, f)
        with open(os.path.join(d, "events.jsonl"), "w") as f:
            for a in ["12,345.60", "-1,000.00", "7.50", "1,000,000.01"]:
                f.write(json.dumps({"customer": "z", "amount": a}) + chr(10))
        sync.run(d)
        sync.run(d)
        with open(os.path.join(d, "balances.json")) as f:
            z = json.load(f)["balances"]["z"]
        if z != 101135411:
            fails.append("new events give %r cents, want 101135411" % z)
    finally:
        shutil.rmtree(d, ignore_errors=True)
except Exception as e:
    fails.append("balances could not be exercised: %r" % e)
for x in fails:
    print(x, file=sys.stderr)
sys.exit(1 if fails else 0)
