import sys

sys.path.insert(0, ".")
fails = []
try:
    import mailer
    import orders
    import signup
    import smtp

    def reset():
        smtp.OUTBOX.clear()
        smtp.CONNECTIONS.clear()

    reset()
    try:
        orders.notify_shipped({"id": "o1", "email": "ada@example.com"})
    except Exception as e:
        fails.append("notify_shipped still raises %r" % (e,))
    if [m[1] for m in smtp.OUTBOX] != ["ada@example.com"]:
        fails.append("notify_shipped delivered %r" % (smtp.OUTBOX,))
    reset()
    try:
        m = mailer.Mailer("smtp.example.com")
        m.send("ben@example.com", "Hello", "first")
        m.send("ben@example.com", "Hello", "second")
    except Exception as e:
        fails.append("a new Mailer cannot send without being told to connect first: %r; the next caller will forget too" % (e,))
    if [m[3] for m in smtp.OUTBOX] != ["first", "second"]:
        fails.append("two sends from one Mailer delivered %r" % ([m[3] for m in smtp.OUTBOX],))
    elif len(smtp.CONNECTIONS) != 1:
        fails.append("two sends from one Mailer opened %d connections, want 1" % len(smtp.CONNECTIONS))
    reset()
    try:
        signup.welcome("cy@example.com")
    except Exception as e:
        fails.append("the signup email broke: %r" % (e,))
    if [m[1] for m in smtp.OUTBOX] != ["cy@example.com"] or len(smtp.CONNECTIONS) > 1:
        fails.append("the signup email delivered %r over %d connections" % (smtp.OUTBOX, len(smtp.CONNECTIONS)))
except Exception as e:
    fails.append("the mail modules could not be exercised: %r" % (e,))
for f in fails:
    print(f, file=sys.stderr)
sys.exit(1 if fails else 0)
