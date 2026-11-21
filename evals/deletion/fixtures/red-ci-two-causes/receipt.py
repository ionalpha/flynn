import pricing


def receipt(amount_cents):
    fee = pricing.fee_cents(amount_cents)
    return "Amount %d.%02d  Fee %d.%02d  Total %d.%02d" % (
        *divmod(amount_cents, 100),
        *divmod(fee, 100),
        *divmod(amount_cents + fee, 100),
    )
