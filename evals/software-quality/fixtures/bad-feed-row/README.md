# Price import

The supplier drops `feed.csv` every night and the scheduler runs

    python importer.py feed.csv prices.json

Rows are `sku,price`. The storefront reads `prices.json`.
