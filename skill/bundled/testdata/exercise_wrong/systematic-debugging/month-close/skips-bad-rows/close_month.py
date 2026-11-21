import sys

import importer
from vendor import ledger_client


def close(path, db="data/ledger.db"):
    client = ledger_client.Client(db)
    total = 0
    for entry in importer.read_entries(path):
        try:
            client.post(entry["account"], entry["cents"], entry["memo"])
        except ledger_client.LedgerError:
            continue
        total += entry["cents"]
    return total


if __name__ == "__main__":
    print("posted %d cents" % close(sys.argv[1], *sys.argv[2:3]))
