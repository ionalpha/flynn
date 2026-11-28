"""Turns queued jobs into files under OUT_DIR, which the web app serves."""

import os

import queue_store

OUT_DIR = "reports"
REJECTED = []


def _target(title):
    """The file a title names, or ValueError if it leads outside OUT_DIR.

    Checked here, where the path is built, because jobs reach the queue from the API,
    the importer and anything else that can write the file; none of them is the only
    way in.
    """
    root = os.path.realpath(OUT_DIR)
    path = os.path.realpath(os.path.join(root, title + ".txt"))
    if os.path.commonpath([root, path]) != root or path == root:
        raise ValueError("title leaves the reports folder")
    return path


def run_pending():
    """Write each queued report to OUT_DIR/<title>.txt and return the paths written.

    A title like "2026/q1" files the report in a folder. One bad job must not stop the
    jobs behind it.
    """
    done = []
    for job in queue_store.drain():
        try:
            path = _target(str(job.get("title", "")))
        except ValueError:
            REJECTED.append(job.get("title"))
            continue
        os.makedirs(os.path.dirname(path), exist_ok=True)
        with open(path, "w", encoding="utf-8") as f:
            f.write(job["body"])
        done.append(path)
    return done
