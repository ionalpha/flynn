"""Remove expired uploads."""
import argparse
import os
import sys

import catalog


def sweep(root, today):
    """Remove what is due and return the names removed."""
    names = catalog.due(root, today)
    entries = catalog.load(root)
    for name in names:
        os.remove(os.path.join(root, name))
        del entries[name]
    catalog.save(root, entries)
    return names


def main(argv):
    parser = argparse.ArgumentParser()
    parser.add_argument("root")
    parser.add_argument("--today", required=True)
    args = parser.parse_args(argv)
    for name in sweep(args.root, args.today):
        print("removed " + name)


if __name__ == "__main__":
    main(sys.argv[1:])
