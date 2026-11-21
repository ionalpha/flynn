"""A file-backed job queue shared by the API process, the importer and the worker."""

import json
import os

PATH = os.path.join("data", "jobs.jsonl")


def put(job):
    os.makedirs(os.path.dirname(PATH) or ".", exist_ok=True)
    with open(PATH, "a", encoding="utf-8") as f:
        f.write(json.dumps(job) + "\n")


def drain():
    """Return every queued job and empty the queue."""
    if not os.path.exists(PATH):
        return []
    with open(PATH, encoding="utf-8") as f:
        jobs = [json.loads(line) for line in f if line.strip()]
    os.remove(PATH)
    return jobs
