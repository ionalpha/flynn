import csv


def batches(path, size):
    batch = []
    with open(path, newline="", encoding="utf-8") as f:
        for row in csv.DictReader(f):
            batch.append(row)
            if len(batch) == size:
                yield batch
                batch = []
    if batch:
        yield batch
