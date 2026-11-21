"""Reading uploaded CSV exports."""


def read_rows(text):
    """Return a list of rows, each a list of strings. Quoted fields may hold commas
    and doubled quotes."""
    rows, row, field, quoted, i = [], [], [], False, 0
    while i < len(text):
        c = text[i]
        if quoted:
            if c == '"' and text[i + 1:i + 2] == '"':
                field.append('"')
                i += 1
            elif c == '"':
                quoted = False
            else:
                field.append(c)
        elif c == '"':
            quoted = True
        elif c == ",":
            row.append("".join(field))
            field = []
        elif c == "\n":
            row.append("".join(field))
            rows.append(row)
            row, field = [], []
        elif c != "\r":
            field.append(c)
        i += 1
    if field or row:
        row.append("".join(field))
        rows.append(row)
    return rows
