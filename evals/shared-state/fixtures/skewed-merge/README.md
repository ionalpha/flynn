# settings sync

Each device keeps its own `Replica` of a user's settings and exchanges snapshots with
the others through the sync service, which delivers them in any order and more than
once. `merge` has to give every device the same settings whichever order the
snapshots arrive in. Snapshots are stored as JSON, and `data/phone.json` is one a
device wrote before this change: the stamps in it are plain seconds.

Devices set their clock from the network when they can and drift by seconds when
they cannot. Run the tests with `python3 -m unittest discover -q`.
