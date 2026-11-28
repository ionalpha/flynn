"""Files that other teams load."""
import csv

import client


def export_csv(path):
    with open(path, "w", newline="") as f:
        out = csv.writer(f)
        out.writerow(["id", "customer", "cents"])
        for o in client.list_orders()["orders"]:
            out.writerow([o["id"], o["customer"], o["cents"]])
