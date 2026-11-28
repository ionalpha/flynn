"""Push stock counts to the inventory service."""
import sys

import settings


def main(argv):
    values = settings.load(argv[0])
    print("endpoint %s" % values["endpoint"])
    print("waiting up to %d seconds, %d attempts" % (int(values["timeout"]), int(values["retries"])))


if __name__ == "__main__":
    main(sys.argv[1:])
