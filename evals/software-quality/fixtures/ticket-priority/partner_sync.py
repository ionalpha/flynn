"""A copy of a partner's queue job, kept so the export can be tested against it.

The partner owns this file and runs their own copy on their schedule. Do not edit it.
"""
import csv
import sys


def lines(path):
    with open(path, newline="", encoding="utf-8") as f:
        reader = csv.reader(f)
        next(reader)
        for row in reader:
            if row[2] == "open":
                yield "%s\t%s\t%s" % (row[0], row[1], row[3])


if __name__ == "__main__":
    for line in lines(sys.argv[1]):
        print(line)
