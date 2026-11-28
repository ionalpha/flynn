import jobs


def submit_job(payload):
    cfg = jobs.JobConfig(**payload)
    cfg.validate()
    return jobs.enqueue(cfg)
