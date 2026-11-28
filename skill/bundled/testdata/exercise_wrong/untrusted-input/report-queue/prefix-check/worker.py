import os

import queue_store

OUT_DIR = "reports"


def run_pending():
    done = []
    for job in queue_store.drain():
        path = os.path.abspath(os.path.join(OUT_DIR, job["title"] + ".txt"))
        if not path.startswith(os.path.abspath(OUT_DIR)):
            continue
        os.makedirs(os.path.dirname(path), exist_ok=True)
        with open(path, "w", encoding="utf-8") as f:
            f.write(job["body"])
        done.append(path)
    return done
