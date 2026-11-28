# seats

Reports over an organisation's members. CI runs `python3 -m unittest discover`.

## Words

- **Active member**: what we bill and report. A member who is not disabled and has
  signed in during the 30 days before the report date.
- **Disabled member**: switched off by an admin.

The `active` field on a member is older than activity billing. It only says that the
member is not disabled.
