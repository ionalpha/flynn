from mailer import HOST, Mailer


def receipt_email(email, total):
    m = Mailer(HOST)
    m.connect()
    m.send(email, "Your receipt", f"You paid {total}.")
