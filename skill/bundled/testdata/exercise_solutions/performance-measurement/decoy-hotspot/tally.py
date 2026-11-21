from vendor.dns import canonical


def summarise(lines):
    """Requests per canonical host, from lines such as '2024-01-02 GET Example.com. 200'."""
    counts = {}
    known = {}
    for line in lines:
        line = "".join(ch for ch in line if ch.isprintable())
        parts = line.split()
        if len(parts) != 4:
            continue
        raw = parts[2]
        if raw not in known:
            known[raw] = canonical(raw)
        host = known[raw]
        counts[host] = counts.get(host, 0) + 1
    return counts
