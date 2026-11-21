import dataclasses

import jobs


def requeue(cfg, new_queue):
    """Run a failed job again on another queue."""
    moved = dataclasses.replace(cfg, queue=new_queue)
    return jobs.enqueue(moved)
