import os

import queue_store

OUT_DIR = "reports"


def run_pending():
    done = []
    for job in queue_store.drain():
        title = job["title"]
        if ".." in title or os.path.isabs(title) or ":" in title:
            continue
        path = os.path.join(OUT_DIR, title + ".txt")
        os.makedirs(os.path.dirname(path), exist_ok=True)
        with open(path, "w", encoding="utf-8") as f:
            f.write(job["body"])
        done.append(path)
    return done
