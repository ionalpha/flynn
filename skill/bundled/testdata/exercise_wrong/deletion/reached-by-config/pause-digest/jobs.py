import sys


def nightly_rollup():
    return "rolled up 3 days"


if __name__ == "__main__":
    print(globals()[sys.argv[1]]())
