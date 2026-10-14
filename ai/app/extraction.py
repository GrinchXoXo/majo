"""
Extraction logic for Majo's AI service.

Per PRD 1 §13 and Middleman_Technical_Spec_v1 §3, this module is the
implementation behind the AIProvider Go interface — it can change entirely
without the Go side ever knowing.

Two extraction strategies are supported:

1. HeuristicExtractor — a rule-based, zero-cost extractor tuned for
   Nigerian informal WhatsApp English. This is the default and requires no
   API key, matching the "near-zero AI budget" requirement for the pilot
   (Middleman_Technical_Spec_v1 §4). It is intentionally conservative: it
   never invents a field it can't find (PRD 1 §17).

2. AnthropicExtractor — an optional LLM-backed extractor used only when
   ANTHROPIC_API_KEY is set in the environment. If the call fails for any
   reason (missing/invalid key, network error, malformed response), the
   service automatically falls back to the heuristic extractor rather than
   losing the buyer's message (PRD 1 §20: AI failure must degrade
   gracefully, never lose the message).
"""
import json
import os
import re
from dataclasses import dataclass, field
from typing import List, Optional


@dataclass
class ExtractionResult:
    category: Optional[str] = None
    product: Optional[str] = None
    variant: Optional[str] = None
    budget_min: Optional[int] = None
    budget_max: Optional[int] = None
    location: Optional[str] = None
    condition: Optional[str] = None
    quantity: Optional[int] = None
    urgency: Optional[str] = None
    preferences: List[str] = field(default_factory=list)
    confidence: float = 0.0
    # Phase 4C hardening: interpretation signals for the deterministic
    # policy (Go). The extractor only REPORTS them; it never decides.
    #   other_products: further products named in the same message
    #   ambiguities: "condition_conflict" (new AND used, not "new or used"),
    #                "location_alternatives" (two places offered as options)
    other_products: List[str] = field(default_factory=list)
    ambiguities: List[str] = field(default_factory=list)


# --- Reference data for the heuristic extractor ---
# Deliberately small and hand-curated for the pilot (PRD 1 §36); expand
# using real anonymized pilot conversations once they exist.

CATEGORY_KEYWORDS = {
    "electronics": [
        "iphone", "samsung", "android", "laptop", "macbook", "tablet", "ipad",
        "ps5", "ps4", "playstation", "xbox", "console", "tv", "television",
        "headphone", "headphones", "airpods", "earpiece", "earpieces",
        "phone", "smartphone", "charger", "power bank", "speaker",
        "tecno", "infinix", "itel", "redmi", "xiaomi", "oppo", "nokia", "pixel",
    ],
    "furniture": [
        "mattress", "bed", "table", "chair", "fan", "wardrobe", "cupboard",
        "fridge", "refrigerator", "freezer", "cooker", "microwave",
        "kitchen", "sofa", "shelf",
    ],
    "fashion": [
        "shoe", "shoes", "sneaker", "sneakers", "clothes", "clothing",
        "shirt", "bag", "watch", "dress", "wig", "perfume",
    ],
}

PRODUCT_HINTS = [
    "iphone 13", "iphone 12", "iphone 14", "iphone 15", "iphone 11",
    "iphone", "samsung galaxy", "samsung", "macbook pro", "macbook air",
    "macbook", "laptop", "ipad", "tablet", "ps5", "ps4", "playstation",
    "xbox", "airpods", "airpod", "tv", "television", "mattress", "bed",
    "table", "chair", "office chair", "fan", "wardrobe", "fridge",
    "refrigerator", "sofa", "shoe", "shoes", "sneakers", "bag", "watch",
    "smartphone", "phone",
    # Phase 4C hardening: every category keyword is also a product word (a
    # "freezer" was categorised but never read as the product), plus common
    # household items.
    "freezer", "cooker", "microwave", "cupboard", "shelf", "headphones", "headphone",
    "earpiece", "charger", "power bank", "speaker", "console", "dress", "wig", "perfume",
    "shirt", "clothes", "generator", "inverter", "solar panel", "blender", "iron", "air conditioner",
    "gas cylinder", "washing machine",
    # Phone brands common in Nigeria (Phase 4C hardening).
    "tecno", "infinix", "itel", "redmi", "xiaomi", "oppo", "nokia", "pixel",
]

# A brand directly followed by a product noun qualifies it ("samsung tv" is
# one TV, not a Samsung phone and a TV).
BRAND_HINTS = {"samsung", "tecno", "infinix", "nokia", "xiaomi"}

NIGERIAN_LOCATIONS = [
    "benin city", "benin", "lagos", "abuja", "otuoke", "port harcourt",
    "ibadan", "kano", "enugu", "warri", "uyo", "calabar", "asaba",
    "yenagoa", "owerri", "abeokuta", "uniben", "fuotuoke",
]

URGENCY_PATTERNS = [
    (re.compile(r"\basap\b", re.I), "urgent"),
    (re.compile(r"\burgent(ly)?\b", re.I), "urgent"),
    (re.compile(r"\btoday\b", re.I), "urgent"),
    (re.compile(r"\bnow\b", re.I), "urgent"),
    (re.compile(r"\bimmediately\b", re.I), "urgent"),
]

# Matches "400k", "350-400k", "350k-400k", "around 400k", "under 150k",
# "200k max", "budget is 350k", "for 250k". Amounts are assumed to be in
# thousands of naira when suffixed with "k", which is standard Nigerian
# WhatsApp shorthand.
RANGE_PATTERN = re.compile(r"(\d+(?:\.\d+)?)\s*k?\s*(?:-|to|and)\s*(\d+(?:\.\d+)?)\s*k", re.I)
SINGLE_AMOUNT_PATTERN = re.compile(r"(\d+(?:\.\d+)?)\s*k\b", re.I)
UNDER_PATTERN = re.compile(r"\bunder\s+(\d+(?:\.\d+)?)\s*k\b", re.I)
MAX_PATTERN = re.compile(r"(\d+(?:\.\d+)?)\s*k\s*max\b", re.I)
# Nigerian vendor shorthand often drops the "k" in casual replies
# ("for 640" meaning ₦640,000) — only applied as a last-resort fallback,
# and only for plausible price-shaped bare numbers (2-3 digits).
BARE_PRICE_PATTERN = re.compile(r"\bfor\s+(\d{2,3})\b(?!\s*k)", re.I)
# Explicit Naira symbol / comma-formatted amounts are literal Naira
# values, NOT multiplied by 1000 (unlike the "k" shorthand above):
# "₦620,000", "620,000", or a bare 5-7 digit number like "620000".
NAIRA_SYMBOL_PATTERN = re.compile(r"₦\s*([\d,]{4,})")
COMMA_THOUSANDS_PATTERN = re.compile(r"\b(\d{1,3}(?:,\d{3})+)\b")
BARE_LITERAL_AMOUNT_PATTERN = re.compile(r"\b(\d{5,7})\b")

QUANTITY_PATTERN = re.compile(r"\b(\d+)\s+(?:\w+\s+){0,2}?(?:" + "|".join([
    "iphone", "phone", "chair", "table", "mattress", "bed", "fan",
    "laptop", "shoe", "shoes", "bag",
]) + r")s?\b", re.I)

_NUMBER_WORDS = {
    "one": 1, "two": 2, "three": 3, "four": 4, "five": 5,
    "six": 6, "seven": 7, "eight": 8, "nine": 9, "ten": 10,
}
QUANTITY_WORD_PATTERN = re.compile(
    r"\b(" + "|".join(_NUMBER_WORDS.keys()) + r")\s+(?:\w+\s+){0,2}?(?:" + "|".join([
        "iphone", "phone", "chair", "table", "mattress", "bed", "fan",
        "laptop", "shoe", "shoes", "bag",
    ]) + r")s?\b", re.I,
)

CONDITION_USED_FINE_PATTERN = re.compile(r"used\s+(is\s+)?(fine|ok|okay)|used\s+or\s+new", re.I)
CONDITION_USED_PATTERN = re.compile(r"\b(fairly\s+used|used|second[\s-]?hand|fairly used)\b", re.I)
CONDITION_NEW_PATTERN = re.compile(r"\b(brand\s+new|new)\b", re.I)


def _to_naira(amount_str: str) -> int:
    return int(float(amount_str) * 1000)


def extract_budget(text: str) -> tuple[Optional[int], Optional[int]]:
    m = RANGE_PATTERN.search(text)
    if m:
        lo, hi = _to_naira(m.group(1)), _to_naira(m.group(2))
        return min(lo, hi), max(lo, hi)

    m = UNDER_PATTERN.search(text)
    if m:
        return None, _to_naira(m.group(1))

    m = MAX_PATTERN.search(text)
    if m:
        return None, _to_naira(m.group(1))

    m = SINGLE_AMOUNT_PATTERN.search(text)
    if m:
        val = _to_naira(m.group(1))
        return val, val

    m = BARE_PRICE_PATTERN.search(text)
    if m:
        val = _to_naira(m.group(1))
        return val, val

    # Literal-Naira formats (NOT multiplied by 1000): "₦620,000",
    # "620,000", bare "620000".
    m = NAIRA_SYMBOL_PATTERN.search(text)
    if m:
        val = int(m.group(1).replace(",", ""))
        return val, val

    m = COMMA_THOUSANDS_PATTERN.search(text)
    if m:
        val = int(m.group(1).replace(",", ""))
        return val, val

    m = BARE_LITERAL_AMOUNT_PATTERN.search(text)
    if m:
        val = int(m.group(1))
        return val, val

    return None, None


def extract_location(text: str) -> Optional[str]:
    lowered = text.lower()
    for loc in NIGERIAN_LOCATIONS:
        if loc in lowered:
            # Normalize to a canonical display form.
            if loc in ("benin", "benin city"):
                return "Benin City"
            if loc == "uniben":
                return "Benin City"
            return loc.title()
    return None


def _word_pattern(term: str) -> "re.Pattern[str]":
    # Whole word, optional plural, a digit may follow ("iphone13"):
    # "fan" must not match "fancy", "bag" not "baggage", "table" not
    # "vegetable", "watch" not "watching", "bed" not "bedroom".
    return re.compile(r"(?<![a-z])" + re.escape(term) + r"(?:s|es)?(?![a-z])", re.I)


_HINT_PATTERNS = [(h, _word_pattern(h)) for h in PRODUCT_HINTS]
_CATEGORY_PATTERNS = {cat: [_word_pattern(k) for k in kws] for cat, kws in CATEGORY_KEYWORDS.items()}


def find_products(text: str) -> List[str]:
    """Every distinct product hint named in the text, in order of
    appearance; a hint inside a longer one found ("iphone" inside
    "iphone 13") is not a second product."""
    spans = []
    for hint, pat in _HINT_PATTERNS:
        for m in pat.finditer(text):
            spans.append((m.start(), m.end(), hint))
    spans.sort(key=lambda x: (x[0], -(x[1] - x[0])))
    out, covered = [], []
    for start, end, hint in spans:
        if any(start >= a and end <= b for a, b in covered):
            continue
        covered.append((start, end))
        if hint not in out:
            out.append(hint)
    return out


def extract_category_and_product(text: str) -> tuple[Optional[str], Optional[str]]:
    product = None
    for hint, pat in _HINT_PATTERNS:
        if pat.search(text):
            product = hint
            break
    if product in BRAND_HINTS:
        m = _word_pattern(product).search(text)
        if m:
            rest = text[m.end():m.end() + 24]
            for hint, pat in _HINT_PATTERNS:
                if hint not in BRAND_HINTS and hint != product:
                    nm = pat.match(rest.lstrip())
                    if nm:
                        product = product + " " + hint
                        break

    category = None
    for cat, pats in _CATEGORY_PATTERNS.items():
        if any(p.search(text) for p in pats):
            category = cat
            break

    return category, product


_FAMILY = {"iphone": "phone", "samsung": "phone", "smartphone": "phone", "television": "tv",
           "refrigerator": "fridge", "sneakers": "shoe", "shoes": "shoe", "airpod": "airpods",
           "macbook": "laptop", "office chair": "chair"}


def _family(p: str) -> str:
    head = p.split(" ")[0]
    return _FAMILY.get(p, _FAMILY.get(head, head))


def other_products(text: str, product: Optional[str]) -> List[str]:
    """Products named besides the main one ("iphone and laptop"). A more
    specific name of the same thing ("phone ... iphone 13") or a brand
    qualifying the product ("samsung tv") is not another product."""
    if not product:
        return []
    parts = product.split(" ")
    return [p for p in find_products(text) if p != product and p not in parts and _family(p) != _family(product)
            and not (p in product or product in p)]


_CONDITION_EITHER = re.compile(r"\b(new|used)\s+or\s+(new|used|fairly used)\b|\beither\b|\bany condition\b|used\s+(is\s+)?(fine|ok|okay)", re.I)
_NEW = re.compile(r"\b(brand\s+new|new)\b", re.I)
_USED = re.compile(r"\b(fairly\s+used|used|second[\s-]?hand|tokunbo)\b", re.I)


def extract_ambiguities(text: str) -> List[str]:
    out = []
    if _NEW.search(text) and _USED.search(text) and not _CONDITION_EITHER.search(text):
        out.append("condition_conflict")
    lowered = text.lower()
    places = []
    for loc in NIGERIAN_LOCATIONS:
        if re.search(r"(?<![a-z])" + re.escape(loc) + r"(?![a-z])", lowered):
            canon = "Benin City" if loc in ("benin", "benin city", "uniben") else loc.title()
            if canon not in places:
                places.append(canon)
    if len(places) >= 2:
        out.append("location_alternatives")
    return out


def extract_variant(text: str, product: Optional[str]) -> Optional[str]:
    m = re.search(r"\b(\d+)\s?gb\b", text, re.I)
    if m:
        return f"{m.group(1)}GB"
    return None


def extract_condition(text: str) -> Optional[str]:
    if CONDITION_USED_FINE_PATTERN.search(text):
        return "used_or_new"
    if CONDITION_USED_PATTERN.search(text):
        return "used"
    if CONDITION_NEW_PATTERN.search(text):
        return "new"
    return None


def extract_quantity(text: str) -> Optional[int]:
    for m in QUANTITY_PATTERN.finditer(text):
        # "iphone 13 and a laptop": 13 is the iPhone's model, not a quantity.
        before = text[:m.start()].rstrip().lower()
        if any(before.endswith(h) for h in PRODUCT_HINTS):
            continue
        return int(m.group(1))
    m = QUANTITY_WORD_PATTERN.search(text)
    if m:
        return _NUMBER_WORDS[m.group(1).lower()]
    return None


def extract_urgency(text: str) -> Optional[str]:
    for pattern, label in URGENCY_PATTERNS:
        if pattern.search(text):
            return label
    return None


class HeuristicExtractor:
    """Zero-cost, rule-based extraction. Default for the pilot."""

    def extract(self, text: str) -> ExtractionResult:
        category, product = extract_category_and_product(text)
        budget_min, budget_max = extract_budget(text)
        location = extract_location(text)
        condition = extract_condition(text)
        quantity = extract_quantity(text)
        urgency = extract_urgency(text)
        variant = extract_variant(text, product)
        others = other_products(text, product)
        ambiguities = extract_ambiguities(text)

        # Confidence is informational only (PRD 1 §18) — never used to
        # auto-route in Phase 1. Weighted by how many signals were found.
        confidence = 0.0
        if product:
            confidence += 0.45
        if budget_min or budget_max:
            confidence += 0.25
        if location:
            confidence += 0.15
        if condition:
            confidence += 0.05
        if quantity:
            confidence += 0.05
        if urgency:
            confidence += 0.05
        confidence = round(min(confidence, 0.95), 2)

        return ExtractionResult(
            category=category,
            product=product,
            variant=variant,
            budget_min=budget_min,
            budget_max=budget_max,
            location=location,
            condition=condition,
            quantity=quantity,
            urgency=urgency,
            preferences=[],
            confidence=confidence,
            other_products=others,
            ambiguities=ambiguities,
        )


ANTHROPIC_EXTRACTION_SYSTEM_PROMPT = """You extract structured buyer intent \
from informal Nigerian WhatsApp messages for a procurement platform called \
Majo. Respond ONLY with a JSON object matching this exact schema, no \
other text:

{
  "category": string or null,
  "product": string or null,
  "variant": string or null,
  "budget_min": integer or null (in Naira),
  "budget_max": integer or null (in Naira),
  "location": string or null,
  "condition": string or null ("new", "used", or "used_or_new"),
  "quantity": integer or null,
  "urgency": string or null,
  "preferences": array of strings,
  "confidence": float between 0 and 1,
  "other_products": array of strings (further products named besides "product"),
  "ambiguities": array of strings, from "condition_conflict" (asks for new AND used),
                 "location_alternatives" (offers two places as options)
}

Rules:
- Never invent a value that isn't clearly stated or strongly implied.
- If information is missing, use null rather than guessing.
- "k" suffix means thousands of Naira (e.g. "400k" = 400000).
- Nigerian English, pidgin, and abbreviations are expected input.
"""


class AnthropicExtractor:
    """
    LLM-backed extractor using the Anthropic API. Only used when
    ANTHROPIC_API_KEY is present. Per Middleman_Technical_Spec_v1 §3,
    this should be the "cheapest reliable" model — Haiku-class models are
    the appropriate choice for this task's cost/quality tradeoff.
    """

    def __init__(self, api_key: str):
        self.api_key = api_key

    def extract(self, text: str) -> ExtractionResult:
        import httpx

        resp = httpx.post(
            "https://api.anthropic.com/v1/messages",
            headers={
                "x-api-key": self.api_key,
                "anthropic-version": "2023-06-01",
                "content-type": "application/json",
            },
            json={
                "model": "claude-haiku-4-5-20251001",
                "max_tokens": 500,
                "system": ANTHROPIC_EXTRACTION_SYSTEM_PROMPT,
                "messages": [{"role": "user", "content": text}],
            },
            timeout=10.0,
        )
        resp.raise_for_status()
        data = resp.json()
        raw_text = "".join(
            block["text"] for block in data["content"] if block["type"] == "text"
        )
        parsed = json.loads(raw_text)
        known = set(ExtractionResult.__dataclass_fields__)
        return ExtractionResult(**{k: v for k, v in parsed.items() if k in known})


def get_extractor():
    """
    Composition point: returns the AnthropicExtractor if a key is
    configured, otherwise falls back to the free heuristic extractor.
    Callers should still catch exceptions from .extract() and fall back
    to the heuristic result — see main.py.
    """
    api_key = os.getenv("ANTHROPIC_API_KEY")
    if api_key:
        return AnthropicExtractor(api_key)
    return HeuristicExtractor()


# --- Phase 2: Supply signal extraction ---

@dataclass
class SupplyExtractionResult:
    category: Optional[str] = None
    product: Optional[str] = None
    variant: Optional[str] = None
    condition: Optional[str] = None
    price: Optional[int] = None
    quantity: Optional[int] = None
    location: Optional[str] = None
    availability_status: str = "AVAILABLE"
    confidence: float = 0.0


CAN_SOURCE_PATTERN = re.compile(
    r"\bcan\s+(get|source)\b|\bi\s+can\s+get\b|\bwill\s+source\b"
    r"|\bfit\s+source\b|\bi\s+fit\s+source\b|\bgo\s+get\b|\bi\s+go\s+get\b",
    re.I,
)
UNAVAILABLE_PATTERN = re.compile(
    r"\bno\s+stock\b|\bnot\s+available\b|\bdon'?t\s+have\b|\bno\s+dey\b|\bsold\s+out\b"
    r"|\bnothing\b|\bfinish(?:ed)?\b",
    re.I,
)


def extract_supply(text: str) -> SupplyExtractionResult:
    """
    Heuristic extraction for vendor supply submissions, e.g.
    "I get iPhone 13 128gb, clean one, 620k." (PRD 2 §13, §60).
    Reuses the buyer-request extraction primitives since the underlying
    patterns (product, variant, price, condition, location) are the same
    vocabulary — only the interpretation differs.
    """
    category, product = extract_category_and_product(text)
    variant = extract_variant(text, product)
    condition = extract_condition(text)
    quantity = extract_quantity(text)
    location = extract_location(text)

    # Vendor messages usually state a single price, not a range — reuse
    # the same amount patterns and take the resolved value.
    budget_min, budget_max = extract_budget(text)
    price = budget_max or budget_min

    if UNAVAILABLE_PATTERN.search(text):
        availability = "UNAVAILABLE"
    elif CAN_SOURCE_PATTERN.search(text):
        availability = "CAN_SOURCE"
    else:
        availability = "AVAILABLE"

    confidence = 0.0
    if product:
        confidence += 0.5
    if price:
        confidence += 0.3
    if condition:
        confidence += 0.1
    if availability != "AVAILABLE":
        confidence += 0.1
    confidence = round(min(confidence, 0.95), 2)

    return SupplyExtractionResult(
        category=category,
        product=product,
        variant=variant,
        condition=condition,
        price=price,
        quantity=quantity,
        location=location,
        availability_status=availability,
        confidence=confidence,
    )


# --- Phase 2 / 2.1: Vendor response extraction ---

@dataclass
class VendorResponseExtractionResult:
    # AVAILABLE | UNAVAILABLE | CAN_SOURCE | ALTERNATIVE | UNCLASSIFIED
    status: str = "UNCLASSIFIED"
    # NEEDS_REVIEW | REVIEWED — set by the classifier itself; CORRECTED is
    # only ever set later, by an operator edit (see routing.Service).
    review_state: str = "NEEDS_REVIEW"
    product: Optional[str] = None
    variant: Optional[str] = None
    condition: Optional[str] = None
    price: Optional[int] = None
    quantity: Optional[int] = None
    location: Optional[str] = None
    notes: Optional[str] = None
    confidence: float = 0.0


# Affirmative availability signals. Deliberately does NOT include a bare
# price — PRD 2.1 §2 is explicit that "a price is not evidence of
# availability by itself" ("620k" alone must be UNCLASSIFIED, not
# AVAILABLE). Includes common Nigerian pidgin: "i get am" ("I have it"),
# "e dey" ("it's available"), "i get" alone, "have it"/"i have".
AVAILABLE_PATTERN = re.compile(
    r"\b(yes|yeah|yup)\b|\bavailable\b"
    r"|\bi\s*(?:'ve|\s+have)\b|\bhave\s+it\b"
    r"|\bi\s+get\s+am\b|\bi\s+get\b|\be\s+dey\b",
    re.I,
)
ALTERNATIVE_PATTERN = re.compile(
    r"\bno\s+dey\s+but\b|\bdon'?t\s+have\s+that\s+but\b|\binstead\b|\bonly\s+have\b", re.I
)
ANTHROPIC_VENDOR_RESPONSE_SYSTEM_PROMPT = """You classify a Nigerian WhatsApp vendor's reply to a product \
availability request for a procurement platform called Majo. Respond \
ONLY with a JSON object matching this exact schema, no other text:

{
  "status": "AVAILABLE" | "UNAVAILABLE" | "CAN_SOURCE" | "ALTERNATIVE" | "UNCLASSIFIED",
  "product": string or null,
  "variant": string or null,
  "condition": string or null,
  "price": integer or null (in Naira),
  "quantity": integer or null,
  "location": string or null,
  "notes": string or null,
  "confidence": float between 0 and 1
}

Rules:
- Use UNCLASSIFIED whenever the vendor's intent is genuinely ambiguous —
  a bare price alone, a question, a hedge like "maybe", or a bare time
  reference are NOT evidence of availability or unavailability.
- A price mentioned alone is never sufficient evidence for AVAILABLE.
- Nigerian pidgin is common: "I get am"/"e dey" mean available; "no dey"
  means unavailable; "I fit source"/"I go get" mean CAN_SOURCE.
- "k" suffix means thousands of Naira (e.g. "620k" = 620000).
- Never invent a value that isn't clearly stated or strongly implied.
"""


class AnthropicVendorResponseExtractor:
    """
    Optional LLM-backed vendor-response classifier, used only when
    ANTHROPIC_API_KEY is set (PRD 2.1 §9: AI is optional, never required).
    """

    def __init__(self, api_key: str):
        self.api_key = api_key

    def extract(self, text: str) -> VendorResponseExtractionResult:
        import httpx

        resp = httpx.post(
            "https://api.anthropic.com/v1/messages",
            headers={
                "x-api-key": self.api_key,
                "anthropic-version": "2023-06-01",
                "content-type": "application/json",
            },
            json={
                "model": "claude-haiku-4-5-20251001",
                "max_tokens": 300,
                "system": ANTHROPIC_VENDOR_RESPONSE_SYSTEM_PROMPT,
                "messages": [{"role": "user", "content": text}],
            },
            timeout=10.0,
        )
        resp.raise_for_status()
        data = resp.json()
        raw_text = "".join(b["text"] for b in data["content"] if b["type"] == "text")
        parsed = json.loads(raw_text)
        review_state = "NEEDS_REVIEW" if parsed.get("status") == "UNCLASSIFIED" else "REVIEWED"
        return VendorResponseExtractionResult(review_state=review_state, **parsed)


def get_vendor_response_ai_extractor() -> Optional[AnthropicVendorResponseExtractor]:
    """Returns None (meaning "run heuristic only") when no API key is
    configured — vendor response classification must work standalone."""
    api_key = os.getenv("ANTHROPIC_API_KEY")
    if api_key:
        return AnthropicVendorResponseExtractor(api_key)
    return None


def classify_vendor_response(text: str) -> tuple[VendorResponseExtractionResult, str]:
    """
    Runs the heuristic extractor, and — if configured — the AI extractor,
    then reconciles them per PRD 2.1 §9. Returns (result, extraction_method).

    Reconciliation rules:
    - AI unavailable/failed -> heuristic result only, method="heuristic".
    - Both agree on status -> that status, method="hybrid", REVIEWED
      (unless both said UNCLASSIFIED, which stays NEEDS_REVIEW).
    - Disagree -> never blindly trust the AI. Fall back to the more
      conservative outcome (UNCLASSIFIED unless the heuristic itself was
      confident and non-UNCLASSIFIED, in which case prefer the
      heuristic's deterministic, explainable result over the LLM's).
      Always NEEDS_REVIEW when they disagree, method="hybrid".
    """
    heuristic_result = extract_vendor_response(text)

    ai_extractor = get_vendor_response_ai_extractor()
    if ai_extractor is None:
        return heuristic_result, "heuristic"

    try:
        ai_result = ai_extractor.extract(text)
    except Exception:  # noqa: BLE001 — AI failure must never lose the message or force UNAVAILABLE
        return heuristic_result, "heuristic"

    if heuristic_result.status == ai_result.status:
        agreed = ai_result if ai_result.confidence >= heuristic_result.confidence else heuristic_result
        agreed.review_state = "NEEDS_REVIEW" if agreed.status == "UNCLASSIFIED" else "REVIEWED"
        return agreed, "hybrid"

    # Disagreement: do not blindly trust the AI (PRD 2.1 §9). Prefer
    # whichever side is UNCLASSIFIED (the conservative outcome); if
    # neither is, we still can't safely pick a winner between two
    # different confident-but-conflicting business claims.
    original_heuristic_status = heuristic_result.status
    if heuristic_result.status == "UNCLASSIFIED" or ai_result.status == "UNCLASSIFIED":
        conservative = heuristic_result if heuristic_result.status == "UNCLASSIFIED" else ai_result
        conservative.review_state = "NEEDS_REVIEW"
        conservative.notes = (conservative.notes or "") + " [heuristic/AI disagreement]"
        return conservative, "hybrid"

    heuristic_result.status = "UNCLASSIFIED"
    heuristic_result.review_state = "NEEDS_REVIEW"
    heuristic_result.notes = f"[disagreement: heuristic={original_heuristic_status} ai={ai_result.status}]"
    return heuristic_result, "hybrid"


def extract_vendor_response(text: str) -> VendorResponseExtractionResult:
    """
    Heuristic extraction for vendor replies to a VendorRequest
    (PRD 2 §34-38, refined by PRD 2.1 §1-4).

    Classification is a strict, ordered set of pattern checks — the first
    one that matches wins. Anything matching none of them is UNCLASSIFIED,
    never guessed into AVAILABLE or UNAVAILABLE. This is the core PRD 2.1
    correction: the previous implementation's `else` branch defaulted to
    UNAVAILABLE, which is a false negative business claim exactly as
    unsafe as a false positive. There is no safe default status for text
    the system doesn't understand — only UNCLASSIFIED is safe.

    Order matters: ALTERNATIVE and UNAVAILABLE are checked before the
    broad AVAILABLE pattern so that e.g. "no dey" doesn't fall through to
    an "available" match on some other fragment of the same message.
    """
    stripped = text.strip()

    category, product = extract_category_and_product(text)
    variant = extract_variant(text, product)
    condition = extract_condition(text)
    quantity = extract_quantity(text)
    location = extract_location(text)
    budget_min, budget_max = extract_budget(text)
    price = budget_max or budget_min

    if ALTERNATIVE_PATTERN.search(text):
        status = "ALTERNATIVE"
    elif UNAVAILABLE_PATTERN.search(text):
        status = "UNAVAILABLE"
    elif CAN_SOURCE_PATTERN.search(text):
        status = "CAN_SOURCE"
    elif AVAILABLE_PATTERN.search(text):
        status = "AVAILABLE"
    else:
        # No affirmative, negative, or can-source language recognized.
        # A bare price ("620k"), a question ("How much?", "Which one?"),
        # a hedge ("Maybe."), or a bare time reference ("Tomorrow.") all
        # land here. PRD 2.1 §2: "unknown/ambiguous text must never
        # default to UNAVAILABLE" — UNCLASSIFIED + NEEDS_REVIEW is the
        # only safe outcome, regardless of whether a price happened to be
        # mentioned.
        return VendorResponseExtractionResult(
            status="UNCLASSIFIED",
            review_state="NEEDS_REVIEW",
            product=product, variant=variant, condition=condition,
            price=price, quantity=quantity, location=location,
            notes=stripped or None,
            confidence=0.0,
        )

    # Confidence reflects how much structured signal backs the
    # classification — informational only, never a substitute for
    # review_state (PRD 2.1 §1/§10). Unlike the previous implementation,
    # a confidently-matched UNAVAILABLE now also earns confidence, since
    # "no stock" is just as clear a signal as "yes, 620k".
    confidence = 0.4  # a pattern matched at all, deterministically
    if price:
        confidence += 0.3
    if product or variant:
        confidence += 0.2
    if condition:
        confidence += 0.05
    confidence = round(min(confidence, 0.95), 2)

    return VendorResponseExtractionResult(
        status=status,
        review_state="REVIEWED",
        product=product,
        variant=variant,
        condition=condition,
        price=price,
        quantity=quantity,
        location=location,
        notes=None,
        confidence=confidence,
    )
