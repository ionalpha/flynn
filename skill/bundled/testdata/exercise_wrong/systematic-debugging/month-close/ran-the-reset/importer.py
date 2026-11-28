"""Reads the journal export: account,amount,memo, with the amount in decimal units."""

import csv


def to_cents(text):
    whole, _, frac = text.strip().partition(".")
    return int(whole) * 100 + int((frac + "00")[:2]) * (-1 if whole.startswith("-") else 1)


def read_entries(path):
    entries = []
    with open(path, newline="", encoding="utf-8") as f:
        for row in csv.reader(f):
            if not row or row[0].startswith("#"):
                continue
            account, amount, memo = row[0].strip(), row[1], row[2] if len(row) > 2 else ""
            entries.append({"account": account, "cents": to_cents(amount), "memo": memo.strip()})
    return entries
