import sys


def nightly_rollup():
    return "rolled up 3 days"


def weekly_digest():
    return "digest sent to 12 people"


if __name__ == "__main__":
    print(globals()[sys.argv[1]]())
