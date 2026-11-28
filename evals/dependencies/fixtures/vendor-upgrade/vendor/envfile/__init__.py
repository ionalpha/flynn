"""Read KEY=VALUE files."""

import re

__version__ = "1.2.0"


def parse(text):
    values = {}
    for line in text.splitlines():
        line = line.strip()
        if not line or line.startswith("#") or "=" not in line:
            continue
        key, value = line.split("=", 1)
        value = value.strip()
        if len(value) >= 2 and value[0] == value[-1] and value[0] in "\"'":
            value = value[1:-1]
        # local: ${NAME} refers to a key defined above
        value = re.sub(r"\$\{(\w+)\}", lambda m: values.get(m.group(1), m.group(0)), value)
        values[key.strip()] = value
    return values
