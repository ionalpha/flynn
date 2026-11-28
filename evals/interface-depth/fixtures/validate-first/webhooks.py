import jobs


def on_event(event):
    if event.get("type") != "job.requested":
        return None
    d = event["data"]
    cfg = jobs.JobConfig(
        queue=d.get("queue", ""),
        priority=d.get("priority", 5),
        retries=d.get("retries", 0),
    )
    cfg.validate()
    return jobs.enqueue(cfg)
