# Inventory sync

    python sync.py sync.ini

Settings come from the `[sync]` section of the ini file. Any of them can be
overridden with an environment variable named `SYNC_` plus the setting in capitals.

| Setting | Default | Meaning |
|---|---|---|
| endpoint | https://inventory.internal | where to sync to |
| timeout | 30 | seconds to wait for a reply |
| retries | 3 | attempts before giving up |

## Installs

Each warehouse host has its own `sync.ini`, copied from `sync.example.ini`, and its
own service unit, copied from `deploy/sync.service`. People there edit both. An
upgrade replaces the code and leaves those two files as they are.

Run the tests with `python -m unittest`.
