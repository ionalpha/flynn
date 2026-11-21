import sys

import api
import export
import store


def main(argv):
    rows = store.all_rows()
    if argv[1] == "export":
        print("exported %d rows" % len(export.export(rows)))
    elif argv[1] == "page":
        print("page %d rows" % len(api.page(rows, int(argv[2]))))
    else:
        sys.exit("unknown command")


if __name__ == "__main__":
    main(sys.argv)
