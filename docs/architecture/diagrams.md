# Majo flow diagrams

Mermaid diagrams (reuse-audit R0: GitHub and most Markdown viewers render
` ```mermaid ` blocks; no tool to install). They summarise behaviour that is
specified in prose elsewhere; when the two disagree, the code and the prose
docs win and this file must be corrected.

## 1. Buyer request: from message to confirmed request

Source: `internal/request/request.go`, `internal/workflow/assess.go`,
`docs/architecture/autonomous-runtime.md`.

```mermaid
stateDiagram-v2
    [*] --> EXTRACTING: buyer message with a product
    EXTRACTING --> NEEDS_REVIEW: fields extracted (or extraction failed)
    NEEDS_REVIEW --> CONFIRMED: Assess = clear (autonomous), or operator confirms
    NEEDS_REVIEW --> AWAITING_BUYER: Assess = missing_detail / ambiguous / conflicting\n(Majo asks one question)
    AWAITING_BUYER --> AWAITING_BUYER: answer merged, next question
    AWAITING_BUYER --> CONFIRMED: answer makes it clear, or operator confirms
    AWAITING_BUYER --> NEEDS_REVIEW: operator correction
    CONFIRMED --> [*]: matched and routed to vendors
```

A restricted item (`policy_exception`) is not confirmed: it stays unconfirmed
with an open exception. Correcting a CONFIRMED request keeps it CONFIRMED
(`RequestRepository.UpdateFields`).


Only a CONFIRMED request may be matched, routed, offered or selected
(`request.ErrNotConfirmed`). Greetings and thank-yous never create a request.

## 2. Vendor request lifecycle

Source: `internal/routing/routing.go`, `internal/workflow/timeout.go`.

```mermaid
stateDiagram-v2
    [*] --> PENDING: routed (canonical routing policy)
    PENDING --> SENT: WhatsApp accepted the message\n(deadline job VENDOR_REQUEST_TIMEOUT = sent_at + TTL)
    PENDING --> SEND_FAILED: synchronous provider error,\nor still PENDING after 10 min (startup sweep)
    SENT --> SEND_FAILED: provider reports failure later (webhook)
    SENT --> RESPONDED: vendor replies before the deadline
    SENT --> EXPIRED: deadline job wins the conditional update
    SENT --> CANCELLED: request superseded / closed
    EXPIRED --> EXPIRED: late reply inside AUTO_LATE_REPLY_WINDOW\nstill becomes an offer
    SEND_FAILED --> [*]: recovery decides next step
    EXPIRED --> [*]: recovery decides next step
    RESPONDED --> [*]: response → offer
```

## 3. Recovery decision (`RecoverRequest`)

Runs under the request's workflow lock after an expiry, a send failure, a
decline or exhausted retries. Every outcome is recorded as a
`RECOVER_REQUEST` decision.

```mermaid
flowchart TD
    A[Trigger: expiry / send failure / decline / retries exhausted] --> H{Request paused?}
    H -- yes --> X1[Record only: held]
    H -- no --> C{Still CONFIRMED and not fulfilled?}
    C -- no --> X2[Record only: nothing to recover]
    C -- yes --> O{Buyer already has a usable option?}
    O -- yes --> X3[Record only]
    O -- no --> W{Other vendors still awaited,\nor a resend pending?}
    W -- yes --> X4[Record only: wait]
    W -- no --> S{Superseded, or same state already handled?}
    S -- yes --> X5[Record: duplicate_recovery_prevented]
    S -- no --> F{AUTO_RECOVER_STALLED_REQUESTS on?}
    F -- no --> X6[Record only]
    F -- yes --> L{Rounds and vendor cap left,\nauto-routing on?}
    L -- no --> T[Tell the buyer the honest status;\nrequest stays CONFIRMED]
    L -- yes --> M[Re-match, excluding vendors already asked]
    M --> R{Eligible vendors?}
    R -- in area --> RV[Contact them; tell the buyer]
    R -- only out of area --> B{Buyer consented before?\nCategory not excluded?}
    B -- ask --> Q[Ask the buyer once]
    B -- no --> T
    R -- none --> T
```

## 4. Outbound failure handling

Source: `internal/channel/failure.go`, `docs/architecture/whatsapp.md`.

```mermaid
flowchart LR
    E[Provider error\nsync or webhook] --> K[channel.Classify\nMeta code + HTTP status]
    K --> TR[TRANSIENT / RATE_LIMITED /\nPROVIDER_UNAVAILABLE]
    K --> NR[INVALID_RECIPIENT /\nINVALID_REQUEST / UNKNOWN]
    K --> PW[POLICY: 24h window /\nwait for recipient]
    K --> PA[POLICY: account /\nAUTH_OR_CONFIG]
    TR --> RT[Bounded retries with class delays] --> EX{Retries left?}
    EX -- no --> EXC[Exception + recovery to other vendors;\nbuyer notice kept for next message]
    NR --> EXC2[Exception; vendor: recovery to others]
    PW --> PN[Buyer: keep notice / offers until the buyer writes\nVendor: exception + other vendors]
    PA --> CH[One channel exception]
```

## 5. Operator access (reuse-audit R1)

Source: `internal/httpserver/operator_auth.go`, `internal/access`,
`cmd/operatorctl`.

```mermaid
sequenceDiagram
    participant B as Browser (dashboard)
    participant API as Majo API
    participant DB as operators table
    B->>API: GET /api/... (Origin: dashboard)\nAuthorization: Bearer <token>
    API->>API: CORS: origin in CORS_ALLOWED_ORIGINS?
    API->>API: sha256(token) == OPERATOR_API_TOKEN hash?
    alt shared break-glass token
        API-->>B: 200 (actor "operator", role OPERATOR)
    else named operator
        API->>DB: active operator with this token hash?
        DB-->>API: name, role (VIEWER / OPERATOR)
        alt GET route, or role OPERATOR
            API-->>B: 200 (actor = operator's name and ID in events)
        else VIEWER on a change
            API-->>B: 403 FORBIDDEN
        end
    else unknown, revoked or missing token
        API-->>B: 401 + WWW-Authenticate: Bearer
    end
```
