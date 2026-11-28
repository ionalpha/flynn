"""Files that other teams load."""
import csv

import client


def export_csv(path):
    with open(path, "w", newline="") as f:
        out = csv.writer(f)
        out.writerow(["id", "customer", "cents"])
        cursor = None
        while True:
            page = client.list_orders(cursor)
            for o in page["orders"]:
                out.writerow([o["id"], o["customer"], o["cents"]])
            cursor = page["next_cursor"]
            if len(page["orders"]) < client.PAGE_SIZE:
                return
