# FleetForce provisioning

Maps every organisation in the evision ClickHouse `organization` table to a
Flexprice customer and creates one plan per organisation from a rate card.

## What it creates

Tenant-wide (once):

| Feature lookup key | Meter | Event | Filters |
|---|---|---|---|
| `asset_<ff>_active`, `asset_<ff>_hibernation` for 2w, 3w, 4w, truck, battery | COUNT_UNIQUE `asset_code`, resets per billing period | `asset_billing_state` | `form_factor`, `state` |
| `challan_fetch_billable` | COUNT | `challan_fetch` | `billable = "true"` |
| `map_hits_google` | COUNT | `map_hit` | `provider = "google"` |
| `map_hits_osm` | boolean feature, no meter | – | – |
| `tara_ai_tokens` | SUM of `tokens` | `tara_ai_usage` | – |

Per organisation:

- Customer: `external_id` = org `code`, name = org `name`, billing currency INR,
  metadata `evision_org_code` / `evision_org_type`.
- Plan `<product>_<code>` named `<Product> – <org>` (product `FleetForce` or
  `TRUST` from the rate card) with monthly, in-arrears usage prices: two per form factor the
  card lists (active and hibernation), challan overage, Google Maps package and
  TARA AI token package and a boolean
  entitlement enabling Open Source Maps for free. If the org's product changes,
  the existing plan is renamed in place and keeps its prices.
- With `-subscribe`: a monthly calendar-cycle subscription starting at local
  midnight of the day the org's first billable asset was onboarded (from
  `asset_x_organization`, same statuses as `-asset-statuses`); an org with no
  assets starts on the first day of the current month, and `-start-date` forces
  one date for every org. Plan prices carry that date as `start_date`, because a
  line item starts at the later of its subscription and its price and usage
  before the line item start is never billed; an existing price that starts
  later is terminated at its own start (so it never bills) and recreated. A backdated start makes the period-update
  cron bill every elapsed month, so backfill `-emit-assets -from <that date>`
  and run `-update-periods -apply` (db mode) to raise those invoices;
  `-preview-usage -from -to` prices a window without writing. Per-asset usage
  charges are not prorated by Flexprice, so a mid-month start is billed a full
  month for its stub first period; `-prorate-first-period -apply` then issues an
  adjustment credit note for the unserved days (day-based, like Flexprice's
  fixed-charge proration), once per invoice. Invoices raised by a catch-up
  run are numbered and dated at the time of the run, so `-redate-invoices
  -apply` (db mode, whole environment) renumbers every finalized invoice in
  period order within the month of its period end, sets the issue date to the
  period end (the 1st of the following month) with the due date counted from
  it, and moves the number sequences past the numbers used; the live cron then
  carries on from there. `-purge-zero-invoices -apply` (db mode, whole
  environment) hard-deletes the zero-total SKIPPED invoices that subscription
  creation and usage-free cycles leave behind, and `-purge-cancelled-invoices
  -apply` (db mode) deletes the invoices and credit notes of the selected orgs'
  cancelled subscriptions, for a clean regeneration after a rate change:
  `-subscribe -resubscribe`, re-emit any events whose meters changed,
  `-purge-cancelled-invoices`, `-update-periods`, `-prorate-first-period`,
  `-redate-invoices`. Customers are created
  with `-timezone` (default Asia/Kolkata) and an existing customer's timezone
  is corrected to it, because a subscription inherits the customer's timezone
  and a calendar cycle anchors to month boundaries in that zone. With a UTC
  customer an IST-midnight start leaves a 5.5-hour stub period that would bill
  a full month of per-asset rates. `-resubscribe` cancels an existing active
  subscription immediately without an invoice and creates a fresh one.

The rate card (`ratecard.json`) has a `default` card (product FleetForce) and
`overrides` keyed by org code (or matched by `match_name` as a case-insensitive
substring of the org name). A card prices only the form factors it lists; the
others are metered but free. A card may carry `effective_from` (YYYY-MM-DD):
the subscription never starts before it, so earlier usage is a free trial, and
`revisions` (each with its own `effective_from` and `asset_monthly`) that
replace the asset rates from that date on. Revisions become dated prices: the
earlier rates end where the next revision starts, the revised price's lookup
key ends in `_from<YYYYMMDD>`, and a subscription created across the change
gets one line item per period, so the invoice for each month uses the rates in
force then. Three-wheelers are metered as L3 or L5 (event property
`category`, set from the asset model: a model matching a fragment in
`three_wheeler_l5_models` is L5, every other three-wheeler is L3); a card can
price `THREE_WHEELER` undivided or `THREE_WHEELER_L3`/`THREE_WHEELER_L5`
separately. Overrides today, both on TRUST: `TAPORQ03UH5` (Bansal Credits
Limited: from 2026-04-01, 2W 25 / 3W 50 / TRUCK 50, hibernation 10) and
`TAPORSI8DRJ` (Accelerated Money for U: from 2026-07-15 Battery 65 / 2W 65 /
3W L3 80 / 3W L5 130 / 4W 165 / Bus 250, from 2026-11-01 L3 100 / L5 145 /
4W 200 / Bus 310, hibernation 10 throughout); Bike Bazar (`TAPOR0HWXWT`) uses
the FleetForce default. On every run the plan's live prices are compared with
the card: a price with a different amount or end date, or one the card no
longer lists, is ended at the subscription start (or at its own later start)
and the card's price created; with `-resubscribe` the org's subscription is
recreated so the new line items apply from the start.

By default only `FLEET_OPERATOR` and `LENDER` organisations are provisioned
(86 of 2787 rows; the rest are individual drivers of type `DCO`). Pass
`-types all` or `-types DCO` to include them.

## Asset-state events from evision

`-emit-assets` reads `asset_x_organization` joined to `asset` for the selected
orgs and emits one `asset_billing_state` event per link per day in
`[-from, -to]` (default: the current month to date), timestamped at local
midnight. A link counts on a day when `onboarding_datetime` is before the end
of the day and `offboarding_datetime` is null or after its start; the state is
`HIBERNATION` while `hibernation_start`/`hibernation_end` cover the day,
otherwise `ACTIVE`. Only links whose `onboarding_status` is in
`-asset-statuses` (default: ONBOARDING_SUCCESS, ACTIVE, HIBERNATED,
DEBOARDING_REQUESTED, DEBOARDED) are billable; pre-onboarding statuses such as
DATA_CLEANUP_DONE are not. Event ids are `evt_asset_<org>_<asset>_<date>`, so
re-running a window is idempotent.

In `-mode db` the events are written to the `events` table and the derived
`meter_usage` rows are written alongside, using the same filter, quantity and
unique-hash rules as the Kafka tracking service (COUNT, COUNT_UNIQUE and SUM
only). In `-mode rest` they go through `POST /v1/events/bulk` and the server's
consumers derive `meter_usage`. Battery chemistries (`BATTERY - LFP` etc.) are folded into form factor
`BATTERY` with the chemistry in a separate property. A form factor without a
price on the org's card is metered but not charged.

An asset linked to two organisations (a lender and a fleet operator) is
emitted for both; each organisation is its own customer.

## Event contract: usage_event/v1 over Kafka

The platform never talks to the billing vendor. Each source (data-insights,
api-tapfin-ai, api-tara-ai) publishes a billing-neutral **usage event** to the
Kafka topic `usage_events` (configurable per environment), keyed by the
organisation code, and Flexprice's `platform_usage` consumer
(`internal/platformusage`) ingests it for the configured tenant and
environment. Swapping the billing platform means writing one new consumer.

```json
{
  "version": 1,
  "id": "evt_asset_taporsi8drj_tapas1_2026-11-03",
  "name": "asset_billing_state",
  "organization_code": "TAPORSI8DRJ",
  "occurred_at": "2026-11-03T00:00:00+05:30",
  "source": "data-insights",
  "properties": {"asset_code": "TAP-AS1", "form_factor": "BATTERY", "state": "ACTIVE"}
}
```

- `id` is stable per occurrence, so a redelivered message is not billed twice.
- `name` is the meter's event name; `organization_code` is the customer's
  external id; `occurred_at` is RFC 3339; `properties` is flat and its values
  are compared as strings by the meters.

Flexprice side: set `FLEXPRICE_PLATFORM_USAGE_ENABLED=true`,
`FLEXPRICE_PLATFORM_USAGE_TENANT_ID`, `FLEXPRICE_PLATFORM_USAGE_ENVIRONMENT_ID`
and, when the platform's Kafka is not Flexprice's own cluster,
`FLEXPRICE_PLATFORM_USAGE_KAFKA_BROKERS` (plus `_USE_SASL`, `_SASL_MECHANISM`,
`_SASL_USER`, `_SASL_PASSWORD`, `_TLS`). Malformed messages go to
`usage_events_dlq`.

Platform side: every source is off until `USAGE_EVENTS_ENABLED=true`
(`USAGE_EVENTS_TOPIC` and `USAGE_EVENTS_SOURCE` are optional); api-tara-ai
also needs `KAFKA_BOOTSTRAP_SERVERS` and the `KAFKA_SASL_*` settings since it
had no Kafka client before.

Event names and properties the rate card meters:

- `asset_billing_state` – one event per asset per day (a daily snapshot).
  Properties: `asset_code`, `form_factor` (`TWO_WHEELER` | `THREE_WHEELER` |
  `FOUR_WHEELER` | `TRUCK` | `BUS` | `BATTERY`), `category` (`L3` | `L5`, three-wheelers
  only), `state` (`ACTIVE` | `HIBERNATION`). The meter counts distinct
  `asset_code` per month, so an asset seen in both states in one month is
  billed once in each state.
- `challan_fetch` – one event per fetch. Properties: `asset_code`,
  `billable` (`"true"` | `"false"`). The emitter marks the first fetch of
  each asset in a calendar month `"false"`; Flexprice bills only `"true"`.
  Per-asset free quotas cannot be expressed as a plan tier, which is why the
  emitter owns that rule.
- `map_hit` – one event per map request. Properties: `provider`
  (`google` | `osm`), `kind` (`geocode` | `reverse_geocode` | `directions`).
  Only `google` is billed, per 1000 hits rounded up.
- `tara_ai_usage` – one event per answered question. Properties: `tokens`
  (number, total tokens for the call). Billed per 1,000,000 tokens, rounded up.
- `tara_ai_mcp_usage` – one event per MCP tool call, `tokens` estimated from
  the result size (`estimate: "chars/4"`). Not priced yet.

## Running

Two write modes:

- `-mode rest` (default): the Flexprice HTTP API, from `FLEXPRICE_API_URL`,
  `FLEXPRICE_API_KEY` and optional `FLEXPRICE_ENVIRONMENT_ID`.
- `-mode db`: the service layer in-process against the database named by the
  repo's `.env` (`FLEXPRICE_POSTGRES_*`, `FLEXPRICE_CLICKHOUSE_*`), scoped by
  `-tenant`, `-environment` and `-user` (recorded as creator). Run from the
  repo root so `config.NewConfig` finds `.env` and `config.yaml`.

The evision side always reads `CLICKHOUSE_EVISION_*`; `-env-file path` loads
them from a dotenv file (for example the data-insights `.env`).

```bash
# 1. see the organisations and which rate card each resolves to
go run ./scripts/tapfin/provision -env-file ../data-insights-2/.env -list-orgs

# 2. dry run (default): prints every customer, plan and price it would create
go run ./scripts/tapfin/provision -env-file ../data-insights-2/.env \
  -mode db -tenant tenant_... -environment env_... -user user_...

# 3. apply, optionally only some orgs, optionally with subscriptions
go run ./scripts/tapfin/provision ... -apply -orgs TAPORQ03UH5
go run ./scripts/tapfin/provision ... -apply -subscribe

# 4. undo: archive the selected orgs' plans and customers (shared features stay)
go run ./scripts/tapfin/provision ... -rollback -exclude TAPORQ03UH5,TAPOR0HWXWT -apply
```

When a price is added to a plan that already has prices, the tool syncs it to
the plan's active subscriptions as a new line item. `-email-template
tech+{code}@tapfin.in` sets an email on customers that have none.

`-orgs` and `-exclude` take org codes or case-insensitive name substrings. The
tool is idempotent: existing customers, plans, prices (by lookup key),
entitlements and active subscriptions are reused, not duplicated. Rollback is
a soft archive through the same service layer and refuses plans or customers
that still have subscriptions.
