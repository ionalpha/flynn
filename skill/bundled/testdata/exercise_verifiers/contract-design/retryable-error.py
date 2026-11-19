import sys

sys.path.insert(0, ".")
fails = []
NEW = "The payment is taking longer than usual. Please try again."


def behaviour(tag):
    calls = []

    def flaky(amount):
        calls.append(amount)
        if len(calls) == 1:
            raise payments.gateway_error("timeout")
        return "charged"

    try:
        if checkout.pay(flaky, 700) != "charged" or len(calls) != 2:
            fails.append(tag + ": a timeout is not retried once")
    except Exception as e:
        fails.append(tag + ": a timeout is not retried: %r" % (e,))
    calls.clear()

    def declined(amount):
        calls.append(amount)
        raise payments.gateway_error("declined")

    try:
        checkout.pay(declined, 700)
        fails.append(tag + ": a decline did not reach the caller")
    except Exception:
        if len(calls) != 1:
            fails.append(tag + ": a decline was retried")


try:
    import checkout
    import payments

    if str(payments.gateway_error("timeout")) != NEW:
        fails.append("the timeout message reads %r" % (str(payments.gateway_error("timeout")),))
    behaviour("as written")
    payments.MESSAGES["timeout"] = "Le paiement prend plus de temps que d'habitude."
    payments.MESSAGES["declined"] = NEW + " The card network did not answer in time."
    behaviour("after support rewrites the messages")
except Exception as e:
    fails.append("payments and checkout are not usable: %r" % (e,))
for f in fails:
    print(f, file=sys.stderr)
sys.exit(1 if fails else 0)
