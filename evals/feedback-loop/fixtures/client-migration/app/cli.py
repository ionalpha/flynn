"""Command line lookup."""
import sys

import client
from cache import Cache


def main(argv):
    print(client.fetch_user(int(argv[1]), True))


def run(uid, flag):
    return Cache().fetch_user(uid)["name"]


if __name__ == "__main__":
    main(sys.argv)
