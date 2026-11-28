import helpers


def render(rows):
    return "\n".join("- [" + ", ".join(helpers.pad_left(c, 4) for c in row) + "]" for row in rows)
