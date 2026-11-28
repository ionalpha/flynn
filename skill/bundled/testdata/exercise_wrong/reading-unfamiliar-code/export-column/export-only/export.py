import csv
import json
import os


def _load(name):
    with open(os.path.join("data", name), encoding="utf-8") as f:
        return json.load(f)


def write_orders(path):
    customers = _load("customers.json")
    os.makedirs(os.path.dirname(path) or ".", exist_ok=True)
    with open(path, "w", newline="", encoding="utf-8") as f:
        out = csv.writer(f)
        out.writerow(["order_id", "customer", "email", "total_cents", "status"])
        for order in _load("orders.json"):
            customer = customers[order["customer_id"]]
            out.writerow([order["id"], customer["name"], customer["email"], order["total_cents"], order["status"]])


if __name__ == "__main__":
    write_orders(os.path.join("out", "orders.csv"))
