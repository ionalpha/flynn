# Architecture

domain/ holds the pricing rules. It must not import db/: the rules are tested and
reused without a database, so they take the data they need as arguments.

db/ is the database access. It may import domain/.

app.py wires the two together. dbtools.py is a set of maintenance scripts, unrelated
to db/ despite the name.
