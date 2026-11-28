import data
import render


def run():
    return render.table(("Region", "Orders", "Cents"), data.SALES)
