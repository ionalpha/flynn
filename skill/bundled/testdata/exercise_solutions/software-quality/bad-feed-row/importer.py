"""Nightly price import: reads a supplier CSV of sku,price and writes prices.json."""
import csv
import json
import sys


def load_prices(path):
    """Return ({sku: price}, [rejected rows]) for the CSV at path.

    A row that is not `sku,price` with a numeric price is rejected, not loaded.
    """
    prices = {}
    rejected = []
    with open(path, newline="", encoding="utf-8") as f:
        for number, row in enumerate(csv.reader(f), start=1):
            try:
                sku, raw = row
                prices[sku] = float(raw)
            except ValueError:
                rejected.append((number, row))
    return prices, rejected


def main(argv):
    src, dest = argv[1], argv[2]
    prices, rejected = load_prices(src)
    with open(dest, "w", encoding="utf-8") as f:
        json.dump(prices, f, sort_keys=True)
    print("imported %d prices" % len(prices))
    for number, row in rejected:
        print("rejected line %d: %r" % (number, row), file=sys.stderr)
    return 1 if rejected else 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
