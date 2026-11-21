import helpers


def render(rows):
    return "\n".join(",".join(helpers.csv_quote(c) for c in row) for row in rows)
