import jobs

SCHEDULES = [
    {"queue": "reports", "priority": 3, "every_mins": 60},
    {"queue": "cleanup", "priority": 8, "every_mins": 1440},
]


def run_due(minute, schedules=None):
    """Enqueue every schedule due at this minute of the day; returns how many."""
    n = 0
    for s in schedules if schedules is not None else SCHEDULES:
        if minute % s["every_mins"] == 0:
            cfg = jobs.JobConfig(queue=s["queue"], priority=s["priority"])
            cfg.validate()
            jobs.enqueue(cfg)
            n += 1
    return n
