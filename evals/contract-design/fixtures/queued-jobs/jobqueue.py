"""An in-process stand-in for the shared job queue."""

_jobs = []


def put(job):
    _jobs.append(dict(job))


def take():
    return _jobs.pop(0) if _jobs else None
