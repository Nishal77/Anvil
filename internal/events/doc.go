// Package events gets an ordered, gap-detectable stream of job events from
// producers to any number of browser subscribers, with replay on
// reconnect.
//
// Publisher writes an event: it's persisted to Postgres first, and only
// after that write commits does it get published to Redis. Postgres is
// always the source of truth; Redis is a fast-path notification only —
// pull it out entirely and event history is still readable, jobs still
// finish.
//
// Hub is the fan-out: it turns Redis's one channel per job into any
// number of per-subscriber channels, and makes sure a slow subscriber
// never blocks delivery to everyone else, and never silently misses an
// event either — it gets a stream_gap marker instead.
package events
