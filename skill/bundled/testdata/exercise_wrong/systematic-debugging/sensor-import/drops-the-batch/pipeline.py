import convert
import loader


class BatchRejected(Exception):
    pass


def run(path, sink, size=250):
    count = 0
    for n, batch in enumerate(loader.batches(path, size), 1):
        try:
            readings = [r for r in map(convert.to_reading, batch) if r is not None]
        except ValueError:
            continue
        sink(readings)
        count += len(readings)
    return count
