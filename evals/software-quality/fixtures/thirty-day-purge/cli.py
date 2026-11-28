"""Command line entry for the nightly jobs."""
import argparse
import datetime
import sys

import db
import retention


def main(argv):
    parser = argparse.ArgumentParser()
    sub = parser.add_subparsers(dest="cmd", required=True)
    purge = sub.add_parser("purge")
    purge.add_argument("--db", required=True)
    purge.add_argument("--now", required=True, type=datetime.date.fromisoformat)
    args = parser.parse_args(argv)
    conn = db.connect(args.db)
    try:
        counts = retention.purge(conn, args.now)
    finally:
        conn.close()
    print("purged sessions=%d audit=%d" % (counts["sessions"], counts["audit"]))


if __name__ == "__main__":
    main(sys.argv[1:])
