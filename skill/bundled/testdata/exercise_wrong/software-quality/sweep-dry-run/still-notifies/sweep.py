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
    parser.add_argument("--dry-run", action="store_true")
    args = parser.parse_args(argv)
    if args.dry_run:
        for name in catalog.due(args.root, args.today, preview=True):
            print("would remove " + name)
        return
    for name in sweep(args.root, args.today):
        print("removed " + name)


if __name__ == "__main__":
    main(sys.argv[1:])
