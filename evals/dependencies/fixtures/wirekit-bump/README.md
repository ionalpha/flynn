# fieldlink

Receives telemetry frames from field units and logs them. `telemetry.py` wraps the two
libraries it depends on: `wirekit` (frame encoding) and `tinylog` (log lines). The units
in the field pad their frames to a fixed size, so frames are decoded without the strict
length check.

## Dependencies

Runtime dependencies are listed in `deps.txt`, one per line, with the version in use.
This sandbox has no network, so the registry is mirrored under `registry/`: each release
of a package is unpacked at `registry/<name>/<version>/`, next to that package's
`CHANGELOG.md`. Run anything against a version by putting its directory on `PYTHONPATH`:

    PYTHONPATH=registry/wirekit/2.3.1:registry/tinylog/0.7.3 python3 -m unittest

`ADVISORY.md` is the security advisory the team was sent. The latest wirekit is 3.1.0 and
the latest tinylog is 0.8.0.
