def to_reading(row):
    raw = row["value"].strip()
    if raw == "":
        return None
    value = float(raw.replace("−", "-"))
    if row["unit"] == "F":
        value = round((value - 32) * 5 / 9, 2)
    return (row["site"], row["ts"], value)
