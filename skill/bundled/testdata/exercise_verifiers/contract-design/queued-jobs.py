import subprocess
import sys

sys.path.insert(0, ".")
fails = []
try:
    import api
    import gateway
    import jobqueue
    import worker

    def charged():
        got = gateway.charges[-1]
        return (got[0], got[1], str(got[2]).lower())

    try:
        api.enqueue_charge({"customer": "eu", "total_cents": 700, "currency": "eur"})
        while True:
            job = jobqueue.take()
            if job is None:
                break
            worker.handle(job)
        if charged() != ("eu", 700, "eur"):
            fails.append("a euro order was charged as %r" % (charged(),))
    except Exception as e:
        fails.append("a euro order raised %r" % (e,))
    try:
        gateway.charges.clear()
        worker.handle({"customer": "old", "amount_cents": 300})
        if charged() != ("old", 300, "usd"):
            fails.append("a job queued before the deploy was charged as %r" % (charged(),))
    except Exception as e:
        fails.append("a job queued before the deploy raised %r (README: jobs wait up to a day)" % (e,))
    run = subprocess.run([sys.executable, "-m", "unittest", "discover", "-q"], capture_output=True, text=True)
    if run.returncode != 0:
        fails.append("the tests fail: " + run.stderr[-1500:])
except Exception as e:
    fails.append("verifier could not run: %r" % (e,))
for f in fails:
    print(f, file=sys.stderr)
sys.exit(1 if fails else 0)
