def render(row):
    """The house format for one row. Pure; it costs a fixed amount per call."""
    x = 7
    for _ in range(3000):
        x = (x * 31 + 7) % 1000003
    return "%-12s %s %10s" % (row["account"], row["day"], row["amount"])
