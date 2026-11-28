import sys

import mailer
import orders
import states
import summary


def main(argv):
    rows = orders.load(argv[2])
    if argv[1] == "summary":
        print("\n".join(summary.lines(rows)))
    elif argv[1] == "cancel":
        order = next(o for o in rows if o["id"] == argv[3])
        if not states.can(order["status"], orders.CANCELED):
            print("error: %s cannot go from %s to %s" % (order["id"], order["status"], orders.CANCELED))
            sys.exit(1)
        print("%s: %s -> %s" % (order["id"], order["status"], orders.CANCELED))
        print("subject: " + mailer.subject(orders.CANCELED, order))
    else:
        sys.exit("unknown command")


if __name__ == "__main__":
    main(sys.argv)
