"""Command line lookup."""
import sys

import client
from cache import Cache


def main(argv):
    print(client.get_user(int(argv[1]), include_deleted=True))


def run(uid, flag):
    return Cache().get_user(uid)["name"]


if __name__ == "__main__":
    main(sys.argv)
