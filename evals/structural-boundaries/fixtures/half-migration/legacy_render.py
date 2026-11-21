def _line(cells, widths):
    return " | ".join(str(c).ljust(w)[:w] for c, w in zip(cells, widths))


def table(headers, rows, widths):
    lines = [_line(headers, widths), "-" * (sum(widths) + 3 * (len(widths) - 1))]
    for row in rows:
        if len(row) != len(headers):
            raise ValueError("row has %d cells for %d columns" % (len(row), len(headers)))
        lines.append(_line(row, widths))
    return "\n".join(lines)
