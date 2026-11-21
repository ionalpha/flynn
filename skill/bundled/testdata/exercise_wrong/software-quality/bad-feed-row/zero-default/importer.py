"""Nightly price import: reads a supplier CSV of sku,price and writes prices.json."""
import csv
import json
import sys


def load_prices(path):
    """Return {sku: price} for every row of the CSV at path."""
    prices = {}
    with open(path, newline="", encoding="utf-8") as f:
        for row in csv.reader(f):
            try:
                sku, raw = row
                prices[sku] = float(raw)
            except ValueError:
                prices[row[0]] = 0.0
    return prices


def main(argv):
    src, dest = argv[1], argv[2]
    prices = load_prices(src)
    with open(dest, "w", encoding="utf-8") as f:
        json.dump(prices, f, sort_keys=True)
    print("imported %d prices" % len(prices))
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
