// Package notification will own outbound WhatsApp (and future channel)
// notifications, dedup, frequency limits, and delivery tracking per
// PRD 0.2 §24-25 ("signal over volume"). Implemented alongside Phase 1
// buyer acknowledgement and offer delivery.
package notification
