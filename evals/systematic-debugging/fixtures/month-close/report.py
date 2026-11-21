import sys

import importer


def balances(path):
    out = {}
    for entry in importer.read_entries(path):
        out[entry["account"]] = out.get(entry["account"], 0) + entry["cents"]
    return out


if __name__ == "__main__":
    for account, cents in sorted(balances(sys.argv[1]).items()):
        print("%-12s %10.2f" % (account, cents / 100))
