import jobs


def submit_job(payload):
    return jobs.safe_enqueue(jobs.JobConfig(**payload))
