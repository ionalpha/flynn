import json
import sys

import amounts


def main(path):
    with open("settings.json") as f:
        kind = json.load(f)["amount_kind"]
    parse = getattr(amounts, "parse_" + kind, None)
    imported = skipped = 0
    with open(path) as f:
        for line in f:
            try:
                if parse is None:
                    raise ValueError("no parser")
                parse(line.strip().split(",")[1])
                imported += 1
            except ValueError:
                skipped += 1
    print("imported %d skipped %d" % (imported, skipped))


if __name__ == "__main__":
    main(sys.argv[1])
