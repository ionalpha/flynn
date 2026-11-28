import csv

with open("out/orders.csv", newline="", encoding="utf-8") as f:
    rows = list(csv.reader(f))[1:]

paid = [row for row in rows if row[3] == "paid"]
print("reconciled %d orders, %d cents" % (len(paid), sum(int(row[2]) for row in paid)))
