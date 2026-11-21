import argparse

import jobs


def main(argv):
    p = argparse.ArgumentParser(prog="jobs")
    p.add_argument("--queue", required=True)
    p.add_argument("--priority", type=int, default=5)
    p.add_argument("--retries", type=int, default=0)
    args = p.parse_args(argv)
    try:
        jobs.enqueue(jobs.JobConfig(queue=args.queue, priority=args.priority, retries=args.retries))
    except ValueError as e:
        print("jobs: %s" % e)
        return 2
    return 0
