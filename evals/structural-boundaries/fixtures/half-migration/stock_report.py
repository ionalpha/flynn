import data
import render


def run():
    return render.table(("Sku", "On hand"), data.STOCK)
