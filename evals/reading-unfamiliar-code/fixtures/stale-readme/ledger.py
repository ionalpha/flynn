ENTRIES = []


def post(account, cents, memo=""):
    """Append an entry for account.

    cents is a whole number of cents from the account's point of view: positive adds to
    the account's balance and negative subtracts from it.
    """
    ENTRIES.append((account, cents, memo))
