"""Read KEY=VALUE files."""

import re

__version__ = "1.3.0"

_QUOTED = re.compile(r"""^(["'])(.*?)\1\s*(?:#.*)?$""")


def parse(text):
    values = {}
    for line in text.splitlines():
        line = line.strip()
        if not line or line.startswith("#") or "=" not in line:
            continue
        key, value = line.split("=", 1)
        value = value.strip()
        quoted = _QUOTED.match(value)
        if quoted:
            value = quoted.group(2)
        else:
            value = re.split(r"\s+#", value, maxsplit=1)[0]
        values[key.strip()] = value
    return values
