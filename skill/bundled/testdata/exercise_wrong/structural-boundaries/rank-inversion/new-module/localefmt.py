import money


class Formatter:
    def fmt(self, cents):
        raise NotImplementedError


class DefaultFormatter(Formatter):
    def fmt(self, cents):
        return money.fmt(cents)


class GermanFormatter(Formatter):
    def fmt(self, cents):
        return money.fmt(cents).replace(",", "#").replace(".", ",").replace("#", ".")


def for_locale(locale):
    return GermanFormatter() if locale == "de" else DefaultFormatter()
