import ast
import subprocess
import sys

sys.path.insert(0, ".")
fails = []
try:
    import api
    import cli
    import cron
    import jobs
    import retry_failed
    import webhooks

    BAD = [
        {"queue": ""},
        {"queue": "   "},
        {"queue": "q", "priority": 10},
        {"queue": "q", "max_runtime_secs": 0},
        {"queue": "q", "retries": -1},
    ]

    def attempt(label, fn):
        before = len(jobs.QUEUE)
        try:
            r = fn()
            if not (label.startswith("cli") and isinstance(r, int) and r != 0):
                fails.append("%s accepted a config the worker never picks up" % label)
        except ValueError:
            pass
        except SystemExit:
            pass
        except Exception as e:
            fails.append("%s raised %r, want ValueError" % (label, e))
        if len(jobs.QUEUE) != before:
            fails.append("%s left a bad config in the queue" % label)

    for bad in BAD:
        attempt("api.submit_job(%r)" % (bad,), lambda: api.submit_job(dict(bad)))
        if "max_runtime_secs" not in bad:
            attempt("webhooks.on_event(%r)" % (bad,), lambda: webhooks.on_event({"type": "job.requested", "data": dict(bad)}))
        attempt("jobs.enqueue(JobConfig(**%r))" % (bad,), lambda: jobs.enqueue(jobs.JobConfig(**bad)))
        if "max_runtime_secs" not in bad and "retries" not in bad:
            sched = [{"queue": bad.get("queue", "q"), "priority": bad.get("priority", 5), "every_mins": 1}]
            attempt("cron.run_due(%r)" % (bad,), lambda: cron.run_due(0, sched))
    base = jobs.JobConfig(queue="ok")
    for q in ("", "  "):
        attempt("retry_failed.requeue to queue %r" % (q,), lambda: retry_failed.requeue(base, q))
    for q, pr in (("", "5"), ("  ", "5"), ("q", "10"), ("q", "-1")):
        attempt("cli.main queue=%r priority=%s" % (q, pr), lambda: cli.main(["--queue", q, "--priority", pr]))
    del jobs.QUEUE[:]
    if api.submit_job({"queue": "mail", "priority": 0}) != 1:
        fails.append("a valid api job was refused")
    if cron.run_due(0, [{"queue": "r", "priority": 9, "every_mins": 5}]) != 1:
        fails.append("a valid scheduled job was refused")
    if webhooks.on_event({"type": "job.requested", "data": {"queue": "w"}}) is None:
        fails.append("a valid webhook job was refused")
    if cli.main(["--queue", "ops", "--retries", "2"]) != 0:
        fails.append("a valid cli job was refused")
    if retry_failed.requeue(jobs.JobConfig(queue="a", retries=1), "b") is None or jobs.QUEUE[-1].queue != "b" or jobs.QUEUE[-1].retries != 1:
        fails.append("a valid requeue lost the job's settings")
    if len(jobs.QUEUE) != 5:
        fails.append("valid jobs: queue holds %d, want 5" % len(jobs.QUEUE))
    for mod in ("api", "cli", "cron", "webhooks", "retry_failed"):
        tree = ast.parse(open(mod + ".py", encoding="utf-8").read())
        for n in ast.walk(tree):
            if isinstance(n, ast.Call):
                name = n.func.attr if isinstance(n.func, ast.Attribute) else getattr(n.func, "id", "")
                if "valid" in name.lower():
                    fails.append("%s.py still validates by hand (%s): the next entry point will forget" % (mod, name))
                    break
    p = subprocess.run([sys.executable, "-m", "unittest", "discover", "-s", ".", "-p", "test_*.py"], capture_output=True, text=True)
    if p.returncode != 0:
        fails.append("the suite fails: " + p.stderr[-300:])
except Exception as e:
    fails.append("the job modules could not be exercised: %r" % (e,))
for f in fails:
    print(f, file=sys.stderr)
sys.exit(1 if fails else 0)
