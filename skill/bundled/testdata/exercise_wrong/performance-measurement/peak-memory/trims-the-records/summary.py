def parse(line):
    parts = line.split(" ", 5)
    if len(parts) < 5:
        return None
    ts, method, path, status, ms = parts[:5]
    if not (status.isdigit() and ms.isdigit()):
        return None
    endpoint = "/".join(path.split("?")[0].split("/")[:3])
    return {"endpoint": endpoint, "status": int(status), "ms": int(ms)}


def load(path):
    with open(path, encoding="utf-8") as f:
        return [r for r in map(parse, f.read().splitlines()) if r is not None]


def p95(values):
    values = sorted(values)
    return values[int(0.95 * (len(values) - 1))]


def summarise(path):
    records = load(path)
    out = {}
    for endpoint in sorted({r["endpoint"] for r in records}):
        mine = [r for r in records if r["endpoint"] == endpoint]
        out[endpoint] = {
            "count": len(mine),
            "errors": sum(1 for r in mine if r["status"] >= 500),
            "p95": p95([r["ms"] for r in mine]),
        }
    return out
