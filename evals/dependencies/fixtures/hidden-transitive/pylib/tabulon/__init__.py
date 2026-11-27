"""Aligned plain-text tables."""

from textmeasure import width


def table(rows, headers):
    cells = [[str(h) for h in headers]] + [[str(c) for c in r] for r in rows]
    sizes = [max(width(r[i]) for r in cells) for i in range(len(headers))]
    return "\n".join(
        "  ".join(c + " " * (sizes[i] - width(c)) for i, c in enumerate(r)).rstrip() for r in cells
    )
