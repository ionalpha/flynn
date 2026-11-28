import jobs


def submit_job(payload):
    return jobs.enqueue(jobs.JobConfig(**payload))
