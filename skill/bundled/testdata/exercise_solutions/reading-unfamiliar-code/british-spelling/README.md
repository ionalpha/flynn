# orders

Order lifecycle: `new`, `paid`, `shipped`, `delivered`, `cancelled`.

The allowed transitions between statuses are defined in `states.py`.

- `python3 cli.py summary FILE` prints how many orders are in each status.
- `python3 cli.py cancel FILE ID` checks the order may be cancelled and prints the notice that would be sent.
