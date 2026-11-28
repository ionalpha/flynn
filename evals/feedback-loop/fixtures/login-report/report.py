"""Count failed logins per user."""
import collections
import sys

from parse import parse


def main(argv):
    failed = collections.Counter()
    with open(argv[0], encoding="utf-8") as f:
        for line in f:
            entry = parse(line)
            if entry and entry[1] == "fail":
                failed[entry[0]] += 1
    for user, count in sorted(failed.items(), key=lambda kv: (-kv[1], kv[0])):
        print("%s: %d" % (user, count))


if __name__ == "__main__":
    main(sys.argv[1:])
