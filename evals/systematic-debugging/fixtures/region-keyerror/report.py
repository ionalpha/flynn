"""Sales totals by region."""

from loaders import load_sales, load_stores


def totals_by_region(stores_path, sales_path):
    regions = load_stores(stores_path)
    totals = {}
    for store, cents in load_sales(sales_path):
        region = regions[store]
        totals[region] = totals.get(region, 0) + cents
    return totals


if __name__ == "__main__":
    for region, cents in sorted(totals_by_region("stores.csv", "sales.csv").items()):
        print(region, cents)
