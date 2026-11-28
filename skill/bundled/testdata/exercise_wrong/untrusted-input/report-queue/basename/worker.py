import os

import queue_store

OUT_DIR = "reports"


def run_pending():
    done = []
    for job in queue_store.drain():
        name = os.path.basename(job["title"].replace("\\", "/"))
        path = os.path.join(OUT_DIR, name + ".txt")
        os.makedirs(OUT_DIR, exist_ok=True)
        with open(path, "w", encoding="utf-8") as f:
            f.write(job["body"])
        done.append(path)
    return done
