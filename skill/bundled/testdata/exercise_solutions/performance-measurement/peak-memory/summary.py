def parse(line):
    parts = line.split(" ", 5)
    if len(parts) < 5:
        return None
    ts, method, path, status, ms = parts[:5]
    if not (status.isdigit() and ms.isdigit()):
        return None
    endpoint = "/".join(path.split("?")[0].split("/")[:3])
    return endpoint, int(status), int(ms)


def p95(values):
    values = sorted(values)
    return values[int(0.95 * (len(values) - 1))]


def summarise(path):
    counts, errors, latencies = {}, {}, {}
    with open(path, encoding="utf-8") as f:
        for line in f:
            rec = parse(line.rstrip("\r\n"))
            if rec is None:
                continue
            endpoint, status, ms = rec
            counts[endpoint] = counts.get(endpoint, 0) + 1
            errors[endpoint] = errors.get(endpoint, 0) + (status >= 500)
            latencies.setdefault(endpoint, []).append(ms)
    return {e: {"count": counts[e], "errors": errors[e], "p95": p95(latencies[e])} for e in sorted(counts)}
