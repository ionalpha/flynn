# importer

`python3 importer.py rows.csv` reads `id,amount` rows and prints how many it imported.
Amounts go through `parse_amount` in `amounts.py`, which returns whole cents.
