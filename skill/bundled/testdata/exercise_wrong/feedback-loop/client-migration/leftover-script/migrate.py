import re, pathlib

for p in pathlib.Path("app").glob("*.py"):
    p.write_text(re.sub(r"fetch_user", "get_user", p.read_text()))
