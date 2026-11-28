# Service configs

Each service reads `config/<service>.ini`. `config/defaults.ini` lists every key a
service config sets, with the value a service gets when it has no reason to differ.
A config sets each of those keys, with a value, and no others: a key the defaults do
not list is a leftover or a typo, and a release must not carry it.

`scripts/` holds the checks the team runs by hand before a release; see its README.
