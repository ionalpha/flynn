"""Read the store list and the sales feed."""

import csv


def load_stores(path):
    with open(path, newline="") as f:
        return {row["store_id"]: row["region"] for row in csv.DictReader(f)}


def load_sales(path):
    with open(path, newline="") as f:
        return [(row["store_id"], int(row["cents"])) for row in csv.DictReader(f)]
