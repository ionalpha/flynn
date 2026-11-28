import json

CANCELED = "cancelled"


def load(path):
    with open(path, encoding="utf-8") as f:
        rows = [json.loads(line) for line in f if line.strip()]
    for row in rows:
        if row["status"] == "canceled":  # spelling stored before the change
            row["status"] = CANCELED
    return rows
