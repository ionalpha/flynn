"""Turns queued jobs into files under OUT_DIR, which the web app serves."""

import os

import queue_store

OUT_DIR = "reports"


def run_pending():
    """Write each queued report to OUT_DIR/<title>.txt and return the paths written.

    A title like "2026/q1" files the report in a folder. One bad job must not stop the
    jobs behind it.
    """
    done = []
    for job in queue_store.drain():
        path = os.path.join(OUT_DIR, job["title"] + ".txt")
        os.makedirs(os.path.dirname(path), exist_ok=True)
        with open(path, "w", encoding="utf-8") as f:
            f.write(job["body"])
        done.append(path)
    return done
