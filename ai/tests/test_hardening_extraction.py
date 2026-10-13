"""Phase 4C hardening: product words are whole words, other products and
ambiguity signals are reported (the Go policy decides what to do)."""
from app.extraction import HeuristicExtractor, extract_supply

h = HeuristicExtractor()


def test_product_words_are_whole_words():
    for text in ["vegetable seller", "baggage rack", "I'm watching football", "bedroom curtains"]:
        assert h.extract(text).product is None, text
    assert h.extract("I need a fancy dress").product == "dress"  # not "fan"


def test_plural_and_attached_digits_still_match():
    assert h.extract("I need bags").product == "bag"
    assert h.extract("I want iphone13 128gb").product == "iphone"


def test_second_product_is_reported_not_dropped():
    r = h.extract("I need iphone 13 and a laptop in lagos")
    assert r.product == "iphone 13"
    assert r.other_products == ["laptop"]


def test_brand_qualifies_the_product():
    r = h.extract("fairly used samsung tv 55 inch")
    assert r.product == "samsung tv"
    assert r.other_products == []


def test_more_specific_name_is_not_another_product():
    assert h.extract("phone, iphone 13 pro max").other_products == []


def test_condition_conflict_and_either():
    assert h.extract("abeg I need ps5 brand new fairly used").ambiguities == ["condition_conflict"]
    assert h.extract("I want brand new or used iphone 12").ambiguities == []
    assert h.extract("used is fine, new iphone 12 also ok").ambiguities == []


def test_location_alternatives():
    assert "location_alternatives" in h.extract("I need tv in lagos or abuja").ambiguities
    assert h.extract("I need tv in benin city").ambiguities == []


def test_nigerian_phone_brands():
    r = h.extract("tecno camon 20 in benin")
    assert r.product == "tecno" and r.category == "electronics" and r.location == "Benin City"


def test_supply_extraction_uses_whole_words_too():
    assert extract_supply("fancy shirts 5k").product == "shirt"  # not "fan"


def test_range_with_and():
    r = h.extract("between 200k and 250k")
    assert (r.budget_min, r.budget_max) == (200000, 250000)


def test_freezer_is_a_second_product():
    assert h.extract("I need fridge and freezer for port harcourt").other_products == ["freezer"]


def test_model_number_is_not_a_quantity():
    assert h.extract("I need an iPhone 13 and a laptop in Benin City").quantity is None
    assert h.extract("I need 2 office chairs").quantity == 2
