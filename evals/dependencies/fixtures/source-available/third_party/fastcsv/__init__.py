"""fastcsv 3.0. See LICENSE."""


def _fc_scan(text):
    import csv
    import io

    return [row for row in csv.reader(io.StringIO(text))]


def parse(text):
    return _fc_scan(text)
