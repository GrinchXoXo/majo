# Vendor onboarding

## WhatsApp invite message (send to a vendor)

> Hello! This is Majo, a WhatsApp service that connects buyers with vendors.
> A buyer tells us what they want, and we send the request to vendors who
> sell that kind of thing. You reply with whether you have it and your
> price. There is no fee to join this pilot. If you are interested, reply
> YES and we will add you. You can leave at any time by replying STOP.

## How it works for vendors

1. **You get a request.** Majo sends you a short WhatsApp message describing
   what a buyer wants — for example: "Used iPhone 13, 128GB, around
   ₦650,000, Benin City."
2. **You reply.** Say if you have it, and your price. Short answers are
   fine: "Yes, 620k" or "No get" or "128 no dey, but 256gb dey 700k."
3. **What happens next.** Majo sends the buyer the offers from you and other
   vendors. If the buyer picks yours, we pass the message on. After that,
   you and the buyer sort out payment and delivery yourselves.

## Simple rules

- Reply honestly — your answer goes straight to the buyer.
- Only quote a price for something you actually have.
- If you don't have it, just say "no" — that is a useful answer too.
- Don't ignore every request for too long; buyers move on quickly.

## What the owner needs to add you

- Your name (or business name)
- Your WhatsApp number
- What you sell (for example: phones, furniture)
- The area you cover (for example: Benin City)

You don't fill any form — the owner adds these details for you.

## For the owner: entering a vendor so requests reach them

These rules come from the 01a code audit. Without them, a vendor can sit in
the system and never get a request.

- **WhatsApp number:** type it any usual way (`0803 123 4567`,
  `+234 803 123 4567`). The dashboard stores it as WhatsApp reports it
  (`2348031234567`) and refuses a number it cannot read or one that
  belongs to another vendor.
- **Categories:** use only these words, because buyer requests are sorted
  into them:
  - `electronics` (or the finer `phones`, `laptops`, `gaming`, `tv`,
    `computers`, `accessories`);
  - `fashion`, `furniture`, `food`, `services`, `school`.

  Anything else ("small chops", "gadgets") is saved with a warning, and no
  request will ever carry it.
- **Primary location:** the pilot area, exactly as `PILOT_DEFAULT_LOCATION`
  is set on the server (for example `Unilag`). Buyers who don't say where
  they are get that location.
- **Service areas:** every other name buyers use for places the vendor
  serves:
  - halls, gates, neighbouring areas (`Akoka`, `Moremi Hall`, `Yaba`);
  - longer forms match too: a buyer at "Unilag main gate" reaches a vendor
    whose area is "Unilag".

  A vendor who hasn't posted any stock yet is contacted only when the buyer
  is in their primary location or one of their service areas.
- **Activate** the vendor after checking them. New vendors start as
  PENDING and get nothing.

## Leaving and coming back

- **STOP** (or "unsubscribe", "remove me") makes the vendor inactive at once.
  They get a confirmation, and no further requests.
- **START** sends the owner a request to switch them back on, shown as a
  `VENDOR_OPT_IN_REQUESTED` event. The owner re-activates them on the
  Vendors page. A blocked vendor stays blocked whatever they text.
