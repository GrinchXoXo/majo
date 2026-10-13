"""
Majo AI Service — Phase 1.

Per PRD 0 FINAL §21-22 and PRD 1 §13-20: this service is called by the Go
backend through the AIProvider boundary. It never owns business state —
Postgres (via Go) remains the source of truth.

Phase 1 adds real extraction (heuristic by default, LLM-backed when
ANTHROPIC_API_KEY is configured) behind the same /extract contract that
Phase 0 established as a placeholder.
"""
import logging
from typing import List, Optional

from fastapi import FastAPI
from pydantic import BaseModel

from app.extraction import (
    HeuristicExtractor,
    get_extractor,
    extract_supply,
    extract_vendor_response,
    classify_vendor_response,
)

from app.errortrack import init_error_tracking

logging.basicConfig(level=logging.INFO)
logger = logging.getLogger("majo-ai")

# Reuse-audit R1 pilot: errors go to a Sentry-protocol tracker when SENTRY_DSN
# is set (scrubbed; never request bodies). Must run before the app is built.
ERROR_TRACKING = init_error_tracking()

app = FastAPI(title="Majo AI Service", version="0.2.0")

_heuristic_fallback = HeuristicExtractor()


class ExtractionRequest(BaseModel):
    text: str
    conversation_id: Optional[str] = None


class ExtractionResult(BaseModel):
    category: Optional[str] = None
    product: Optional[str] = None
    variant: Optional[str] = None
    budget_min: Optional[int] = None
    budget_max: Optional[int] = None
    location: Optional[str] = None
    condition: Optional[str] = None
    quantity: Optional[int] = None
    urgency: Optional[str] = None
    preferences: List[str] = []
    confidence: float = 0.0
    other_products: List[str] = []
    ambiguities: List[str] = []


@app.get("/health")
def health():
    return {"status": "ok"}


@app.post("/extract", response_model=ExtractionResult)
def extract(req: ExtractionRequest) -> ExtractionResult:
    """
    Extracts structured buyer intent from a raw WhatsApp message.

    Per PRD 1 §20: AI failure must degrade gracefully and never lose the
    buyer's message. If the configured extractor (e.g. the Anthropic-backed
    one) raises for any reason, we fall back to the free heuristic
    extractor rather than returning an error — the Go side always gets a
    usable (if less confident) result, and the operator reviews it either way.
    """
    extractor = get_extractor()

    try:
        result = extractor.extract(req.text)
    except Exception as exc:  # noqa: BLE001 - intentionally broad: never lose the message
        logger.warning("primary extractor failed (%s), falling back to heuristic", exc)
        result = _heuristic_fallback.extract(req.text)

    return ExtractionResult(
        category=result.category,
        product=result.product,
        variant=result.variant,
        budget_min=result.budget_min,
        budget_max=result.budget_max,
        location=result.location,
        condition=result.condition,
        quantity=result.quantity,
        urgency=result.urgency,
        preferences=result.preferences,
        confidence=result.confidence,
        other_products=getattr(result, "other_products", []) or [],
        ambiguities=getattr(result, "ambiguities", []) or [],
    )


# --- Phase 2: supply + vendor response extraction ---

class SupplyExtractionRequest(BaseModel):
    text: str
    vendor_id: Optional[str] = None


class SupplyExtractionResponse(BaseModel):
    category: Optional[str] = None
    product: Optional[str] = None
    variant: Optional[str] = None
    condition: Optional[str] = None
    price: Optional[int] = None
    quantity: Optional[int] = None
    location: Optional[str] = None
    availability_status: str = "AVAILABLE"
    confidence: float = 0.0


@app.post("/extract-supply", response_model=SupplyExtractionResponse)
def extract_supply_endpoint(req: SupplyExtractionRequest) -> SupplyExtractionResponse:
    """
    Extracts structured supply info from a vendor's WhatsApp message
    (PRD 2 §13-14). Heuristic-only for now — the same "never lose the
    message" principle applies: this endpoint cannot fail in a way that
    loses data, since the raw message is always persisted by the caller
    regardless of what this returns.
    """
    try:
        result = extract_supply(req.text)
    except Exception as exc:  # noqa: BLE001
        logger.warning("supply extraction failed (%s), returning empty result", exc)
        result = extract_supply("")  # degrade to an empty-but-valid result

    return SupplyExtractionResponse(
        category=result.category,
        product=result.product,
        variant=result.variant,
        condition=result.condition,
        price=result.price,
        quantity=result.quantity,
        location=result.location,
        availability_status=result.availability_status,
        confidence=result.confidence,
    )


class VendorResponseExtractionRequest(BaseModel):
    text: str
    vendor_request_id: Optional[str] = None


class VendorResponseExtractionResponse(BaseModel):
    status: str
    review_state: str
    extraction_method: str
    product: Optional[str] = None
    variant: Optional[str] = None
    condition: Optional[str] = None
    price: Optional[int] = None
    quantity: Optional[int] = None
    location: Optional[str] = None
    notes: Optional[str] = None
    confidence: float = 0.0


@app.post("/extract-vendor-response", response_model=VendorResponseExtractionResponse)
def extract_vendor_response_endpoint(req: VendorResponseExtractionRequest) -> VendorResponseExtractionResponse:
    """
    Classifies a vendor's reply to a VendorRequest (PRD 2 §34-38, refined
    by PRD 2.1). Ambiguous replies ("Maybe.", "Which one?", a bare price)
    are classified UNCLASSIFIED + NEEDS_REVIEW rather than guessed into a
    business status (PRD 2.1 §2). If an AI extractor is configured, its
    output is reconciled against the heuristic per PRD 2.1 §9 — the two
    are never silently allowed to disagree without operator visibility,
    and the AI is never blindly trusted over a more conservative
    heuristic result. On total failure (including AI errors), this
    degrades to UNCLASSIFIED, never UNAVAILABLE (PRD 2.1 §8).
    """
    try:
        result, method = classify_vendor_response(req.text)
    except Exception as exc:  # noqa: BLE001 — must never lose the message or fabricate UNAVAILABLE
        logger.warning("vendor response extraction failed (%s), flagging for review", exc)
        result = extract_vendor_response("")
        method = "heuristic"

    return VendorResponseExtractionResponse(
        status=result.status,
        review_state=result.review_state,
        extraction_method=method,
        product=result.product,
        variant=result.variant,
        condition=result.condition,
        price=result.price,
        quantity=result.quantity,
        location=result.location,
        notes=result.notes,
        confidence=result.confidence,
    )
