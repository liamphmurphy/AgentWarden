---
name: make-platform
description: >-
  Expert reference for Make, the internal customer data platform (CDP). Covers the three-service pipeline architecture (make-ingest → Kafka → make-router → destination topics, make-api as the control plane), core data models (Envelope, Event, Source, Destination, Schema), write key authentication, destination filtering and redaction, ingestocampus S3 caching, HIPAA routing, sensitivity levels, and the Grules bouncer system. Also covers make-dna: the identity/trait/audience service with its gRPC API, trait types, audience evaluation, DynamoDB trait value storage, identity graph, forwarder pattern, and backfill pipeline. Use when working in make-api, make-router, make-ingest, or make-dna repos, or when asked about how Make works, how events flow, how traits/audiences/profiles are computed, how identities are resolved, or how routing and redaction decisions are made.
---

# Make Platform

Make is an internal customer data platform (CDP). Event data flows through three Go services:

```
Client → make-ingest (HTTP) → Kafka ("events" topic) → make-router (Kafka consumer) → destination-specific Kafka topics
                                                                              ↑
                                                         make-api (control plane, MySQL + Redis)
```

## Repos

| Repo | Role | Key entry point |
|------|------|----------------|
| `make-ingest` | HTTP front door; validates events and writes to Kafka | `cmd/server/` |
| `make-router` | Kafka consumer; routes events to per-destination topics | `cmd/consumer/` |
| `make-api` | Control plane REST API; manages sources, destinations, schemas, workspaces, orgs | `cmd/server/` |

## make-ingest

**What it does:** Receives `POST /v2/t` events, validates them, and writes a serialized `core.Envelope` to the `events` Kafka topic.

**Key flow (`track.go`):**
1. Extract write key from `Authorization: Basic <writeKey>` header.
2. Look up `Source` from write key via `sourceStore` (backed by make-api or ingestocampus S3).
3. Decode `core.EventRequest` from JSON body (max 400 KB).
4. Check write key is active; check origin filter; check schema allow/block list.
5. Validate event via `eventRequestValidator` (checks required fields against event schema).
6. Build `core.Envelope` (adds `traceID`, `sourceUID`, `workspaceUID`, source/workspace names, HIPAA flag, IP address).
7. Write `json.Marshal(envelope)` to Kafka via `pipelineWriter`.

**Failover:** If Kafka writes fail past a circuit-breaker threshold, events fall back to an SQS queue (replayed later by a lambda).

**Ingestocampus:** Instead of hitting make-api, make-ingest can load write keys, schemas, and source-workspaces from S3 snapshots (`UseIngestocampus=true`). This decouples ingest resiliency from make-api availability.

**Endpoints:**
- `POST /v2/t` — ingest event
- `POST /v2/validate` — dry-run validation (no Kafka write)
- `GET /health` — health check

## make-router

**What it does:** Consumes from the `events` Kafka topic, applies routing/filtering/redaction logic, and publishes per-destination envelopes to destination-type Kafka topics.

**Key flow (`handler.go` → `HandleMessage`):**
1. Unmarshal `core.Envelope` from Kafka message.
2. Fetch connected destinations for `envelope.SourceUID` from `connectedDestinationsStore` (cached from make-api or ingestocampus).
3. Apply destination filtering via `filterDestinations`:
   - **Global destination filter** — Grules rules per destination type slug.
   - **Destination filters** — Grules rules per destination UID (global, event-scoped, field-removal).
   - **`event.Integrations` map** — per-event opt-in/out by destination type slug or UID.
4. For each allowed destination:
   - Skip if topic is on hard blocklist (`google_ad_manager`, `redshift_spectrum`, `liveintent`).
   - Skip alias events from shadow workspaces.
   - Skip if `event.IsHipaa=true` and destination `ReceivesHIPAAData=false`.
   - Apply **redaction** via `redact.Redact(event, securityPolicies, sensitivityLevel)`.
   - Apply **field removals** if a field-removal bouncer matches.
   - Optionally push to **delay queue** (SQS) if the destination has configured event delays.
   - Publish `core.Envelope{DestinationUID, SourceUID, WorkspaceUID, Event (redacted)}` to `d.DestinationType.Slug` Kafka topic.
5. Special case: `core.redventures.usertracking.ConsentCaptured.v1` always publishes to `privacy_consent` (and `datalake` if not already sent).
6. Publish redacted event to **Redis debugger** (async).

**Cache reload:** Sources, destinations, schemas, and bouncers are reloaded on a ticker (`OptionReloadFrequency`, default 2 min). Ingestocampus S3 is the preferred cache source.

## make-api

**What it does:** REST control plane. All configuration for the pipeline lives here — stored in MySQL, surfaced as JSON APIs consumed by make-ingest, make-router, and the UI.

**Key resource types:**

| Resource | Description |
|----------|-------------|
| **Source** | A named event origin with a write key, type, owner workspace, origin filter, schema filter, HIPAA flag |
| **Destination** | A named sink connected to source(s); has type slug (maps to Kafka topic), sensitivity level, HIPAA flag, event delays |
| **Schema** | Defines an event's required/optional fields and security policies (used for validation in ingest and redaction in router) |
| **Workspace** | Logical grouping of sources and destinations for a team/product |
| **Organization** | Top-level owner of workspaces |
| **Write Key** | Credential attached to a source, used by clients to authenticate ingest requests |

**Config stores (MySQL):** Separate DBs for `make`, `auth`, `sources`, `destinations`, `schemas`, `replay`.

**Auth:** The code supports a multi-verifier setup (Auth0/Marvin, AWS Cognito, Azure AD) but in practice only **Custos** is used for authorization.

**Kafka publishing:** make-api publishes config-change events to Kafka topics (`PublisherDriver=kafka`).

**DNA:** gRPC integration with `make-dna` service for audience/trait/profile functionality. See the [make-dna](#make-dna) section below.

**Other capabilities:** Data Lake IAM management (S3 access points), replay (SQS-backed event replay), privacy/CCPA (OneTrust + privacy-api-sdk), Facebook audience mappings, Databricks expected-reach jobs.

## Core Data Models

```go
// core.Envelope — travels through the entire pipeline
type Envelope struct {
    Event          core.Event
    SourceUID      string
    WorkspaceUID   string
    DestinationUID string   // set by router, empty in ingest
    Metadata       map[string]interface{}
}

// core.Event — the event payload
type Event struct {
    ID, MessageID, TraceID string
    Event                  string          // event type / schema key e.g. "Product Viewed"
    AnonymousID            string
    Properties             json.RawMessage
    Integrations           map[string]bool // opt-in/out per destination slug or UID
    IsHipaa                bool
    SourceUID, SourceName  string
    WorkspaceName          string
    TenantID               string          // legacy
    ReceivedAt             time.Time
}
```

## Sensitivity / Redaction

Three levels (most restricted → least): **restricted** → **private** → **internal**

- Each schema field can have a sensitivity level.
- Each destination type has a `MinSensitivityLevel` and `MaxSensitivityLevel`.
- Each destination instance has a `SensitivityLevel`.
- Router clamps the destination sensitivity level within the destination type's min/max, then calls `redact.Redact` to zero out fields that exceed the allowed level.

## Destination Filtering (Bouncer / Grules)

Filters use a Grules v2 rule engine. Rules are stored in make-api and loaded by make-router on startup + periodic reload.

| Filter type | Scope | Effect |
|-------------|-------|--------|
| Global destination filter | Per destination UID | Blocks entire event from that destination |
| Event destination filter | Per destination UID + event name list | Blocks event from that destination for specific event types |
| Field removal filter | Per destination UID | Removes specified properties from event before publishing |
| Destination type filter | Per destination type slug | Blocks entire event from all destinations of that type |

## Key Kafka Topics

| Topic | Producer | Consumer |
|-------|----------|---------|
| `events` | make-ingest | make-router |
| `datalake` | make-router | `make-datalake-delta` (custom Delta Lake solution) |
| `privacy_consent` | make-router | privacy/consent consumers |
| `<destination_type_slug>` (e.g. `sailthru`, `iterable`, `facebook_audiences`) | make-router | destination-specific Make consumers (e.g. `make-sailthru`, `make-iterable`, `make-facebook-audiences`) |

## make-dna

**What it does:** The customer data / identity platform. Computes audience membership and trait values from ingested events, and exposes them via a gRPC API. make-api calls it for audience/trait/profile functionality.

**Repo:** `make-dna`

**Components:**

| Component | Role |
|-----------|------|
| `cmd/server` | gRPC API server (port `:4000`). Backed by MySQL (trait/audience config) + DynamoDB (trait values, identities, events) + Redis (cache). |
| `cmd/traits-v2-consumer` | Kafka consumer. Reads events from a DNA-specific topic, matches them against trait configs, and upserts trait values to DynamoDB. |
| `cmd/forwarder` | Kafka consumer. Reads computed DNA events (`TraitUpdated`, `AudienceFitted`, `AudienceUnfitted`, `ProfileMerged`) from the `dna-forwarder` topic and re-injects them back into make-ingest as track events. |
| `cmd/identities-consumer` | Kafka consumer. Handles identity graph messages and writes to the DynamoDB identities table. |
| `cmd/events-consumer` | Kafka consumer. Reads raw events and persists them to DynamoDB for event history queries. |
| `cmd/backfill` | Three-stage backfill pipeline (pathfinder → hydrator → processor) for reprocessing historical trait values from S3 coldstore. |

**gRPC service (`dna.proto` → `DNA` service, 59 RPCs):**

Key RPC groups:
- **Identity:** `GetIdentity`, `MergeMakeIDs`, `DeleteIdentityGraph`, `GetAnonymousIDsBySources`
- **Traits:** `CreateTrait`, `GetTrait`, `UpdateTrait`, `DeleteTrait`, `ListTraits`, `EvaluateTraitsV2`, `EvaluateTraitsV2Filtered`
- **Audiences:** `CreateAudience`, `GetAudience`, `UpdateAudience`, `DeleteAudience`, `ListAudiences`, `PauseAudience`, `ResumeAudience`, `UpsertAudienceSizes`
- **Profiles:** `EvaluateProfile`, `EvaluateProfileV2`
- **Portfolios:** `CreatePortfolio`, `GetPortfolio`, `ListPortfolios`
- **Privacy/GDPR:** `DeletePersonalData`, `DeleteTraitsBySources`
- **Schema TTLs:** `CreateSchemaTTL`, `UpdateSchemaTTL`, `DeleteSchemaTTL`

**Trait types** (`pkg/models/trait_type.go`):
`average`, `boolean`, `count`, `exists`, `first`, `funnel`, `last`, `list`, `maximum`, `minimum`, `mostFrequent`, `propensity`, `rollingCount`, `sum`, `uniqueList`

**Audience types** (proto enum `AudienceType`): `custom` (Grules-based criteria over trait values), `lookalike` (Databricks ML model trained against a parent audience).

**Core data flow for trait computation:**
```
make-ingest → Kafka (events topic) → make-router → DNA destination topic
                                                            ↓
                                              cmd/traits-v2-consumer
                                              (match event → trait configs,
                                               upsert DynamoDB trait values)
                                                            ↓
                                              cmd/forwarder publishes
                                              identity.TraitUpdated.v1 /
                                              identity.AudienceFitted.v1 /
                                              identity.AudienceUnfitted.v1 /
                                              identity.ProfileMerged.v1
                                              back to make-ingest (via dna-forwarder Kafka topic)
```

**Trait values storage:** DynamoDB table (`MakeDNATraits` by default). `pkg/traitvalues` repo does per-type upserts using a single `UpdateItem` call (read-modify-write in one round trip). Time-bound trait values store per-upsert TTLs and compute expiration on read; item TTL is bumped on each write.

**Audiences are derived from trait values.** Audience criteria are Grules v2 predicates evaluated against a customer's accumulated trait values. `pkg/traitsv2.ReadService.GetCustomerProfile` returns both trait values and fitted audiences together — because audiences are simply predicates over traits, traits are the authority for both.

**TraitConfig** (`pkg/models/trait_config.go`): defines how to compute a trait from events:
- `OwnerDestinationUID` — the DNA destination that owns the config
- `SyncedDestinationUIDs` — other Make destinations that receive the computed trait events
- `EventType` + `EventRule` (Grules v2) — event filter
- `PropertyPath` — GJSON path to the value to extract (empty for boolean/count/rollingCount)
- `TimeWindow` — rolling window for time-bound trait types
- `FunnelStepConfigs` — ordered steps for `funnel` type

**Identities:** DynamoDB-backed graph linking aliases (anonymous IDs, emails, etc.) by source. The `GetIdentity` RPC resolves an alias to its full linked identity graph. `MergeMakeIDs` merges identity graphs (capped at `MaxMergeableMakeIDs`, default 5).

**Backfill** (`cmd/backfill`): Triggered by invoking the **pathfinder** lambda with `{traitUid, startDate, endDate}`. It reads coldstore S3 files → hydrator converts them to DNA events → processor reprocesses them through the same trait-value upsert logic as the live consumer.

**External integrations:**
- **Databricks**: lookalike audience training jobs and propensity trait ML models
- **Jarvis**: semantic search / global tags
- **Content Platform**: tag URL lookups
- **Privacy API**: GDPR/CCPA delete support

**Auth:** Azure AD (client credentials) for calls to make-api, privacy-api, Jarvis, and Content Platform.

**Cache:** Redis (TTL default 24h) for audience configs looked up during profile evaluation. DynamoDB for all persistent trait/identity data. MySQL (writer + reader DSNs) for trait/audience config storage.

## Ingestocampus

An S3-backed snapshot cache used by both make-ingest and make-router to avoid direct make-api dependency. Snapshots are stored as JSON files in a configured S3 bucket. Enables both services to start and operate without make-api being available.

- make-ingest: write keys, schemas, source-workspaces
- make-router: sources, schemas, destinations, destination filters, destination type filters, sensitivity levels