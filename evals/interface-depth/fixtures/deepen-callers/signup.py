from mailer import HOST, Mailer


def welcome(email):
    m = Mailer(HOST)
    m.connect()
    m.send(email, "Welcome", "Thanks for signing up.")
